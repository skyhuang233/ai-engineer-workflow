package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/skyhuang233/workflow/internal/credential"
	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

// onboardingCommand is intentionally the only public setup surface of a
// versioned CLI. Platform mutation belongs exclusively to workflow-setup.
func onboardingCommand(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("onboarding requires plan, apply, or verify")
	}
	operation := args[0]
	flags := flag.NewFlagSet("onboarding "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	home := flags.String("workflow-home", workflowHomeFromArgs(args), "Workflow Home")
	repositoryPath := flags.String("repo", "", "absolute repository checkout")
	owner := flags.String("owner", "", "GitHub owner")
	apiBase := flags.String("github-api", "", "GitHub API base")
	approvedDigest := flags.String("onboarding-plan-digest", "", "exact approved Onboarding Plan digest")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *repositoryPath == "" {
		*repositoryPath, _ = os.Getwd()
	}
	if !filepath.IsAbs(*repositoryPath) {
		return errors.New("onboarding requires an absolute --repo")
	}
	active, err := launcher.ReadActive(*home)
	if err != nil {
		return err
	}
	if active.Readiness != "ready" {
		return errors.New("onboarding requires a live ready active generation")
	}
	database, err := store.OpenActivated(context.Background(), generationDatabasePath(*home, active))
	if err != nil {
		return err
	}
	defer database.Close()
	ctx := context.Background()
	session, err := onboardingGitHubSession(ctx, database, *home, *repositoryPath, *apiBase)
	if err != nil {
		return err
	}
	if *owner != "" && !strings.EqualFold(*owner, session.owner) {
		return errors.New("onboarding owner differs from the verified plaintext PAT owner")
	}
	switch operation {
	case "plan":
		remote := githubOnboardingRemote{client: session.client, owner: session.owner}
		plan, err := onboarding.Plan(ctx, onboarding.PlanOptions{RepositoryPath: *repositoryPath, WorkflowHome: *home, Owner: session.owner, AuthenticatedLogin: session.login, Remote: githubRemoteHead{remote: remote}, Labels: requiredWorkflowLabels(), Policy: session.client, Publication: session.client, State: onboardingCurrentState{Client: session.client, Store: database}, PlatformReleaseDigest: strings.TrimPrefix(active.BundleDigest, "sha256:")})
		if err != nil {
			return err
		}
		raw, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		canonical, digest, err := setupcontract.Canonicalize(raw)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]any{"onboarding_plan": json.RawMessage(canonical), "onboarding_plan_digest": digest, "active_generation": active.Generation})
	case "apply":
		if *approvedDigest == "" {
			return errors.New("onboarding apply requires --onboarding-plan-digest")
		}
		raw, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		plan, _, _, err := setupcontract.ParsePlan(raw)
		if err != nil {
			var envelope struct {
				OnboardingPlan json.RawMessage `json:"onboarding_plan"`
			}
			if json.Unmarshal(raw, &envelope) == nil && len(envelope.OnboardingPlan) != 0 {
				raw = envelope.OnboardingPlan
				plan, _, _, err = setupcontract.ParsePlan(raw)
			}
		}
		if err != nil {
			return err
		}
		if plan.Kind != setupcontract.RepositoryOnboarding {
			return errors.New("onboarding apply accepts only an Onboarding Plan")
		}
		if err := requireApprovedOnboardingRepositoryPath(*repositoryPath, plan.Target.RepositoryPath); err != nil {
			return err
		}
		client := session.client.WithOnboardingIdentity(session.owner, session.login, plan.Target.GitHubRepository)
		adapter := &onboarding.RepositoryAdapter{Remote: githubOnboardingRemote{client: client, owner: session.owner}, Credential: onboarding.GitCredential{Username: "x-access-token", Token: session.token}, Owner: session.owner, PlanDigest: *approvedDigest, Store: database, RepositoryPath: plan.Target.RepositoryPath}
		result, err := (onboarding.Executor{Store: database, Adapter: adapter}).Apply(ctx, raw, *approvedDigest, strings.TrimPrefix(active.BundleDigest, "sha256:"))
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	case "verify":
		if *approvedDigest == "" {
			return errors.New("onboarding verify requires --onboarding-plan-digest")
		}
		record, err := database.SetupPlanByDigest(context.Background(), *approvedDigest)
		if err != nil {
			return err
		}
		if record.Kind != string(setupcontract.RepositoryOnboarding) {
			return errors.New("stored plan is not Repository Onboarding")
		}
		plan, _, digest, parseErr := setupcontract.ParsePlan([]byte(record.CanonicalJSON))
		if parseErr != nil || digest != *approvedDigest {
			return errors.New("stored Onboarding Plan is not the exact approved digest")
		}
		if err := requireApprovedOnboardingRepositoryPath(*repositoryPath, plan.Target.RepositoryPath); err != nil {
			return err
		}
		client := session.client.WithOnboardingIdentity(session.owner, session.login, plan.Target.GitHubRepository)
		adapter := &onboarding.RepositoryAdapter{Remote: githubOnboardingRemote{client: client, owner: session.owner}, Credential: onboarding.GitCredential{Username: "x-access-token", Token: session.token}, Owner: session.owner, PlanDigest: *approvedDigest, Store: database, RepositoryPath: plan.Target.RepositoryPath}
		for _, effect := range plan.Effects {
			status, _, readErr := adapter.Readback(ctx, effect)
			if readErr != nil || status != setupcontract.EffectSatisfied {
				return errors.New("Repository Onboarding remote or local readback is not admitted")
			}
		}
		if err := setupcontract.VerifyExpectedResults(plan, func() []setupcontract.EffectResult {
			values := make([]setupcontract.EffectResult, 0, len(plan.Effects))
			for _, e := range plan.Effects {
				values = append(values, setupcontract.EffectResult{EffectID: e.ID, Status: setupcontract.EffectSatisfied})
			}
			return values
		}()); err != nil {
			return err
		}
		admission, admissionErr := database.RepositoryAdmission(ctx, record.Projection)
		if admissionErr != nil || !admission.Eligible || admission.OnboardingPlanDigestSHA256 != *approvedDigest {
			return errors.New("Repository Onboarding admission is not live for the exact digest")
		}
		return json.NewEncoder(output).Encode(map[string]any{"status": "repository_onboarding_admitted", "active_generation": active.Generation, "onboarding_plan_digest": record.DigestSHA256, "repository": admission.Repository})
	default:
		return errors.New("unknown onboarding operation")
	}
}

func requireApprovedOnboardingRepositoryPath(requested, approved string) error {
	requestedPath, err := filepath.Abs(requested)
	if err != nil {
		return err
	}
	approvedPath, err := filepath.Abs(approved)
	if err != nil {
		return err
	}
	requestedPath = filepath.Clean(requestedPath)
	approvedPath = filepath.Clean(approvedPath)
	samePath := requestedPath == approvedPath
	if runtime.GOOS == "windows" {
		samePath = strings.EqualFold(requestedPath, approvedPath)
	}
	if !samePath {
		return errors.New("onboarding repository path differs from the approved Onboarding Plan")
	}
	return nil
}

type onboardingSession struct {
	client              *workflowgithub.Client
	owner, login, token string
}

func onboardingGitHubSession(ctx context.Context, database *store.Store, home, repositoryPath, apiBase string) (onboardingSession, error) {
	if database == nil {
		return onboardingSession{}, errors.New("generation-local onboarding store is unavailable")
	}
	verification, err := database.GitHubPATVerification(ctx)
	if err != nil || verification.Status != "verified" {
		return onboardingSession{}, errors.New("onboarding requires a verified plaintext GitHub PAT")
	}
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		return onboardingSession{}, err
	}
	if !strings.EqualFold(filepath.Clean(verification.CredentialPath), filepath.Clean(layout.CredentialFile)) {
		return onboardingSession{}, errors.New("verified GitHub PAT path differs from active Workflow Home")
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(ctx, credential.GatewayTarget)
	if err != nil || credential.Fingerprint(token) != verification.FingerprintSHA256 {
		return onboardingSession{}, errors.New("plaintext GitHub PAT differs from its verified active-generation record")
	}
	live, err := (githubcredential.Verifier{APIBase: apiBase, Client: http.DefaultClient}).Verify(ctx, token, verification.Owner)
	if err != nil {
		return onboardingSession{}, errors.New("GitHub PAT live capability verification failed")
	}
	client := workflowgithub.NewClient(apiBase, token, http.DefaultClient).WithRepositoryOwner(verification.Owner)
	var actor struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	if err := client.RequestJSON(ctx, http.MethodGet, "/user", nil, &actor); err != nil || actor.Type != "User" || !strings.EqualFold(actor.Login, live.Login) {
		return onboardingSession{}, errors.New("GitHub PAT live identity differs from verified owner-bound capability")
	}
	return onboardingSession{client: client, owner: verification.Owner, login: actor.Login, token: token}, nil
}
func argumentValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func workflowHomeFromArgs(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--workflow-home" {
			return args[i+1]
		}
	}
	return filepath.Join(strings.TrimSpace(os.Getenv("LOCALAPPDATA")), "AgentWorkflow")
}

func generationDatabasePath(home string, active launcher.Active) string {
	return filepath.Join(home, "platform", "generations", active.Generation, "workflow.db")
}
