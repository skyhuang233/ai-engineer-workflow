package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/admission"
	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/hostsetup"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	setupengine "github.com/skyhuang233/workflow/internal/setup"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type setupResponse struct {
	Status             string `json:"status"`
	PlatformReady      bool   `json:"platform_ready,omitempty"`
	RepositoryAdmitted bool   `json:"repository_admitted,omitempty"`
	Blocker            string `json:"blocker,omitempty"`
	Result             any    `json:"result,omitempty"`
	PlanPath           string `json:"plan_path,omitempty"`
	DigestSHA256       string `json:"digest_sha256,omitempty"`
	Projection         string `json:"projection,omitempty"`
}

func setupCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("setup requires plan, apply, or verify")
	}
	switch args[0] {
	case "plan":
		return runSetupPlan(args[1:], os.Stdout)
	case "apply":
		return runSetupApply(args[1:], os.Stdin, os.Stdout)
	case "verify":
		return runSetupVerify(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown setup command %q", args[0])
	}
}

func runSetupPlan(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repository := flags.String("repo", "", "absolute target repository")
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	repositoryName := flags.String("repository-name", "", "GitHub name for an unpublished repository")
	visibility := flags.String("visibility", "private", "private or public for an unpublished repository")
	domainLayout := flags.String("domain-layout", "single-context", "single-context or multi-context")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*repository) {
		return errors.New("setup plan requires an absolute --repo")
	}
	if *visibility != "private" && *visibility != "public" {
		return errors.New("setup plan --visibility must be private or public")
	}
	if *domainLayout != "single-context" && *domainLayout != "multi-context" {
		return errors.New("setup plan --domain-layout must be single-context or multi-context")
	}
	private := *visibility == "private"
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(layout.State, "workflow.db")); errors.Is(err, os.ErrNotExist) {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Bootstrap must be completed by the entry skill"})
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	if _, err := database.PlatformInstallation(context.Background()); err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Bootstrap must be completed by the entry skill"})
	}
	if _, err := database.GitHubPATVerification(context.Background()); err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Control Plane GitHub PAT verification is incomplete; rerun the approved Platform Bootstrap Plan"})
	}
	verification, err := database.GitHubPATVerification(context.Background())
	if err != nil {
		return err
	}
	config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
	token, err := verifiedClassicPAT(context.Background(), database, config)
	if err != nil {
		return err
	}
	client := github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
	discovery, discoveryErr := onboarding.Discover(context.Background(), *repository, onboardingGitHubRemote{Client: client})
	if discoveryErr == nil && discovery.Repository != "" {
		if verifyErr := verifyRecordedAdmission(context.Background(), database, layout, client, discovery.Repository); verifyErr == nil {
			return writeSetupResponse(output, setupResponse{Status: "ready", PlatformReady: true, RepositoryAdmitted: true})
		}
	}
	contractRaw, err := os.ReadFile(filepath.Join(layout.Config, "platform-setup-contract.json"))
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", PlatformReady: true, Blocker: "installed Platform Setup Contract is unavailable; repair Platform Bootstrap"})
	}
	var platformContract platformrelease.PlatformSetupContract
	if err := json.Unmarshal(contractRaw, &platformContract); err != nil || platformContract.Validate() != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", PlatformReady: true, Blocker: "installed Platform Setup Contract is invalid; repair Platform Bootstrap"})
	}
	labels := make([]onboarding.Label, 0, len(platformContract.RepositoryContract.Labels))
	for _, label := range platformContract.RepositoryContract.Labels {
		labels = append(labels, onboarding.Label{Name: label.Name, Color: label.Color, Description: label.Description})
	}
	plan, err := onboarding.Plan(context.Background(), onboarding.PlanOptions{RepositoryPath: *repository, WorkflowHome: layout.Root, Owner: verification.Owner, AuthenticatedLogin: verification.Login, RepositoryName: *repositoryName, Private: &private, Remote: onboardingGitHubRemote{Client: client}, PlatformReleaseDigest: mustPlatformDigest(context.Background(), database), Labels: labels, Policy: client, DomainLayout: *domainLayout})
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", PlatformReady: true, Blocker: err.Error()})
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		return err
	}
	planFile, err := os.CreateTemp("", "workflow-onboarding-plan-*.json")
	if err != nil {
		return err
	}
	planPath := planFile.Name()
	if _, err := planFile.Write(canonical); err != nil {
		planFile.Close()
		return err
	}
	if err := planFile.Close(); err != nil {
		return err
	}
	return writeSetupResponse(output, setupResponse{Status: "plan_required", PlatformReady: true, PlanPath: planPath, DigestSHA256: digest, Projection: setupengine.Project(plan, digest)})
}

type onboardingGitHubRemote struct{ Client *github.Client }

func (r onboardingGitHubRemote) Resolve(ctx context.Context, origin string) (string, string, error) {
	repository, err := parseOriginRepository(origin)
	if err != nil {
		return "", "", err
	}
	revision, err := r.Client.DefaultBranchHead(ctx, repository)
	return revision.Name, revision.Head, err
}
func parseOriginRepository(origin string) (string, error) {
	value := strings.TrimSpace(origin)
	if strings.HasPrefix(value, "git@github.com:") {
		return strings.TrimSuffix(strings.TrimPrefix(value, "git@github.com:"), ".git"), nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	return strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), nil
}
func mustPlatformDigest(ctx context.Context, database *store.Store) string {
	value, err := database.PlatformInstallation(ctx)
	if err != nil {
		return strings.Repeat("0", 64)
	}
	return value.ReleaseManifestDigestSHA256
}

func runSetupApply(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("setup apply", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	planPath := flags.String("plan", "", "canonical Setup Plan path")
	approved := flags.String("approved-digest", "", "approved canonical SHA-256 digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *planPath == "" || *approved == "" {
		return errors.New("setup apply requires --plan and --approved-digest")
	}
	raw, err := os.ReadFile(*planPath)
	if err != nil {
		return err
	}
	var target struct {
		Target struct {
			WorkflowHome string `json:"workflow_home"`
		} `json:"target"`
	}
	if err := json.Unmarshal(raw, &target); err != nil {
		return err
	}
	layout, err := workflowhome.Resolve(target.Target.WorkflowHome)
	if err != nil {
		return err
	}
	plan, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		return err
	}
	adapter := setupengine.HostAdapter{Layout: layout, RepositoryPath: plan.Target.RepositoryPath, PlanDigest: digest}
	if plan.Kind == setupcontract.RepositoryOnboarding {
		database, openErr := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
		if openErr != nil {
			return openErr
		}
		verification, readErr := database.GitHubPATVerification(context.Background())
		if readErr != nil {
			database.Close()
			return readErr
		}
		config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
		token, tokenErr := verifiedClassicPAT(context.Background(), database, config)
		database.Close()
		if tokenErr != nil {
			return tokenErr
		}
		adapter.GitHub = github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
		adapter.GitCredential = onboarding.GitCredential{Username: "x-access-token", Token: token}
	}
	engine := setupengine.Engine{Adapter: adapter, SecretInput: &setupengine.SecretInput{Reader: input}}
	result, applyErr := engine.Apply(context.Background(), raw, *approved)
	writeErr := writeSetupResponse(output, setupResponse{Status: string(result.Status), Result: result})
	return errors.Join(applyErr, writeErr)
}

func runSetupVerify(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repository := flags.String("repo", "", "absolute target repository")
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*repository) {
		return errors.New("setup verify requires an absolute --repo")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(layout.State, "workflow.db")); errors.Is(err, os.ErrNotExist) {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Installation state is unavailable"})
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	platformErr := verifyPlatformReady(context.Background(), database, layout)
	platformReady := platformErr == nil
	verification, credentialErr := database.GitHubPATVerification(context.Background())
	repositoryReady := false
	if credentialErr == nil {
		config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
		if token, tokenErr := verifiedClassicPAT(context.Background(), database, config); tokenErr == nil {
			client := github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
			if discovery, discoveryErr := onboarding.Discover(context.Background(), *repository, onboardingGitHubRemote{Client: client}); discoveryErr == nil && discovery.Repository != "" {
				repositoryReady = verifyRecordedAdmission(context.Background(), database, layout, client, discovery.Repository) == nil
			}
		}
	}
	status, blocker := "ready", ""
	if !platformReady || !repositoryReady {
		status = "blocked"
		reasons := []string{}
		if platformErr != nil {
			reasons = append(reasons, "Platform Ready: "+platformErr.Error())
		}
		if !repositoryReady {
			reasons = append(reasons, "Repository Admitted: live contract or admission evidence is unavailable")
		}
		blocker = strings.Join(reasons, "; ")
	}
	return writeSetupResponse(output, setupResponse{Status: status, PlatformReady: platformReady, RepositoryAdmitted: repositoryReady, Blocker: blocker})
}

func verifyPlatformReady(ctx context.Context, database *store.Store, layout workflowhome.Layout) error {
	installation, err := database.PlatformInstallation(ctx)
	if err != nil {
		return err
	}
	verification, err := database.GitHubPATVerification(ctx)
	if err != nil || verification.Status != "verified" {
		return errors.Join(errors.New("Control Plane PAT verification is unavailable"), err)
	}
	config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
	token, err := verifiedClassicPAT(ctx, database, config)
	if err != nil {
		return err
	}
	patResult := (doctor.GitHubPATCheck{Pin: config.GitHub.Credential, Token: token, Verification: verification}).Run(ctx)
	if patResult.Status != doctor.Pass {
		return errors.New(patResult.Summary)
	}
	record, err := controlplane.ReadRuntimeRecord(layout)
	if err != nil {
		return err
	}
	plan, err := database.LatestSetupPlan(ctx, string(setupcontract.PlatformBootstrap))
	if err != nil {
		return err
	}
	observation := (controlplane.Inspector{}).Inspect(ctx, &record)
	if observation.State != controlplane.StateReady || record.PlatformVersion != installation.PlatformVersion || record.ApprovedPlanDigestSHA256 != plan.DigestSHA256 {
		return errors.New("Control Plane process identity, health, version, or approved plan digest differs")
	}
	if _, err := os.Stat(filepath.Join(layout.Bin, workflowhome.ExecutableName)); err != nil {
		return err
	}
	pathReady, err := workflowhome.CurrentUserPathIsReconciled(layout.Bin)
	if err != nil || !pathReady {
		return errors.Join(errors.New("current-user workflow CLI PATH is not reconciled"), err)
	}
	contractRaw, err := os.ReadFile(filepath.Join(layout.Config, "platform-setup-contract.json"))
	if err != nil {
		return err
	}
	var contract platformrelease.PlatformSetupContract
	if err := json.Unmarshal(contractRaw, &contract); err != nil {
		return err
	}
	if err := contract.Validate(); err != nil {
		return err
	}
	userProfile := os.Getenv("USERPROFILE")
	if userProfile == "" {
		return errors.New("USERPROFILE is required to verify the Workflow Skill Bundle")
	}
	skillsRoot := filepath.Join(userProfile, ".agents", "skills")
	verified, err := (workflowhome.Installation{Layout: layout}).VerifyRecordedSkillBundle(skillsRoot, contract.SkillBundle.Version, contract.SkillBundle.ManagedSkills)
	if err != nil || !verified {
		return errors.Join(errors.New("Workflow Skill Bundle does not match the installed release"), err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := hostsetup.VerifyDockerWorker(probeCtx, nil, contract.Worker.Image, layout.State, layout.Workspaces); err != nil {
		return err
	}
	authFile, err := codexauth.ResolveChatGPT(probeCtx)
	if err != nil {
		return err
	}
	result := (doctor.WorkerCodexSessionCheck{Executor: doctor.OSExecutor{}, Image: contract.Worker.Image, AuthFile: authFile}).Run(probeCtx)
	if result.Status != doctor.Pass {
		return errors.New(result.Summary)
	}
	return nil
}

func verifyRecordedAdmission(ctx context.Context, database *store.Store, layout workflowhome.Layout, client *github.Client, repository string) error {
	value, err := database.RepositoryAdmission(ctx, repository)
	if err != nil || !value.Eligible {
		return errors.Join(errors.New("Repository Admission is not eligible"), err)
	}
	contractRaw, err := os.ReadFile(filepath.Join(layout.Config, "platform-setup-contract.json"))
	if err != nil {
		return suspendAdmission(ctx, database, value, err)
	}
	var contract platformrelease.PlatformSetupContract
	if err := json.Unmarshal(contractRaw, &contract); err != nil {
		return suspendAdmission(ctx, database, value, err)
	}
	if err := contract.Validate(); err != nil {
		return suspendAdmission(ctx, database, value, err)
	}
	if err := (admission.GitHubVerifier{Client: client, Contract: contract}).Verify(ctx, value); err != nil {
		return suspendAdmission(ctx, database, value, err)
	}
	value.VerifiedAt = time.Now().UTC()
	value.SuspensionReason = ""
	return database.RecordRepositoryAdmission(ctx, value)
}

func suspendAdmission(ctx context.Context, database *store.Store, value store.RepositoryAdmission, reason error) error {
	value.Eligible = false
	value.SuspensionReason = reason.Error()
	value.VerifiedAt = time.Now().UTC()
	return errors.Join(reason, database.RecordRepositoryAdmission(ctx, value))
}

func writeSetupResponse(output io.Writer, value setupResponse) error {
	return json.NewEncoder(output).Encode(value)
}
