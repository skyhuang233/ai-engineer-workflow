package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/admission"
	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/hostsetup"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
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

var (
	verifyPlatformReadyForSetup     = verifyPlatformReady
	verifyRecordedAdmissionForSetup = verifyRecordedAdmissionReadOnly
	setupInspectionAPIBase          = ""
	setupInspectionHTTPClient       *http.Client
)

func setupCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("setup requires plan, apply, inspect-platform, or verify")
	}
	switch args[0] {
	case "plan":
		return runSetupPlan(args[1:], os.Stdout)
	case "apply":
		return runSetupApply(args[1:], os.Stdin, os.Stdout)
	case "verify":
		return runSetupVerify(args[1:], os.Stdout)
	case "inspect-platform":
		return runSetupInspectPlatform(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown setup command %q", args[0])
	}
}

type platformInspection struct {
	Platform struct {
		InstallationRecorded        bool   `json:"installation_recorded"`
		Version                     string `json:"version,omitempty"`
		ReleaseManifestDigest       string `json:"release_manifest_digest,omitempty"`
		PlatformSetupContractDigest string `json:"platform_setup_contract_digest,omitempty"`
		WorkflowCLISHA256           string `json:"workflow_cli_sha256,omitempty"`
		ReleaseBundledFilesJSON     string `json:"release_bundled_files_json,omitempty"`
		ReleaseBundledFilesDigest   string `json:"release_bundled_files_digest,omitempty"`
		ControlPlanePlanDigest      string `json:"control_plane_plan_digest_sha256,omitempty"`
	} `json:"platform"`
	WorkflowCLI struct {
		Verified bool `json:"verified"`
	} `json:"workflow_cli"`
	GitHubCredential struct {
		Exists     bool     `json:"exists"`
		Verified   bool     `json:"verified"`
		Owner      string   `json:"owner,omitempty"`
		Scopes     []string `json:"scopes,omitempty"`
		Diagnostic string   `json:"diagnostic,omitempty"`
	} `json:"github_credential"`
}

func runSetupInspectPlatform(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup inspect-platform", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	if err := flags.Parse(args); err != nil {
		return err
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: err.Error()})
	}
	defer database.Close()
	facts, inspectErr := inspectPlatform(context.Background(), database, layout)
	status, blocker := "ready", ""
	if inspectErr != nil {
		status, blocker = "blocked", inspectErr.Error()
	}
	return writeSetupResponse(output, setupResponse{Status: status, Blocker: blocker, Result: facts})
}

func inspectPlatform(ctx context.Context, database *store.Store, layout workflowhome.Layout) (platformInspection, error) {
	var facts platformInspection
	installation, installationErr := database.PlatformInstallation(ctx)
	var pins platformPins
	if installationErr == nil {
		pins = platformPins{Version: installation.PlatformVersion, ReleaseManifestDigest: installation.ReleaseManifestDigestSHA256, PlatformSetupContractDigest: installation.PlatformSetupContractDigestSHA256, WorkflowCLISHA256: installation.WorkflowCLISHA256, ReleaseBundledFilesDigest: installation.ReleaseBundledFilesDigestSHA256}
		facts.Platform.InstallationRecorded = true
		facts.Platform.Version = installation.PlatformVersion
		facts.Platform.ReleaseManifestDigest = installation.ReleaseManifestDigestSHA256
		facts.Platform.PlatformSetupContractDigest = installation.PlatformSetupContractDigestSHA256
		facts.Platform.WorkflowCLISHA256 = installation.WorkflowCLISHA256
		facts.Platform.ReleaseBundledFilesJSON = installation.ReleaseBundledFilesJSON
		facts.Platform.ReleaseBundledFilesDigest = installation.ReleaseBundledFilesDigestSHA256
		facts.Platform.ControlPlanePlanDigest = installation.ControlPlanePlanDigestSHA256
	}
	contractRaw, contractErr := os.ReadFile(filepath.Join(layout.Config, "platform-setup-contract.json"))
	_, contractDigest, canonicalErr := setupcontract.Canonicalize(contractRaw)
	if installationErr == nil {
		facts.WorkflowCLI.Verified, _ = (workflowhome.Installation{Layout: layout}).VerifyVersion(pins.Version, pins.WorkflowCLISHA256)
	}
	verification, verificationErr := database.GitHubPATVerification(ctx)
	token, tokenErr := credential.NewFileStore(layout.CredentialFile).Get(ctx, credential.GatewayTarget)
	facts.GitHubCredential.Exists = tokenErr == nil
	if verificationErr == nil {
		facts.GitHubCredential.Owner = verification.Owner
	}
	if tokenErr == nil && verificationErr == nil {
		pin := doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}
		result := (doctor.GitHubPATCheck{Pin: pin, Token: token, Verification: verification, APIBase: setupInspectionAPIBase, Client: setupInspectionHTTPClient}).Run(ctx)
		facts.GitHubCredential.Verified = result.Status == doctor.Pass
		if facts.GitHubCredential.Verified {
			facts.GitHubCredential.Scopes = append([]string(nil), verification.Scopes...)
		}
		if !facts.GitHubCredential.Verified {
			facts.GitHubCredential.Diagnostic = result.Summary
		}
	} else {
		facts.GitHubCredential.Diagnostic = errors.Join(tokenErr, verificationErr).Error()
	}
	var combined error
	if installationErr != nil {
		combined = errors.Join(combined, installationErr)
	}
	if contractErr != nil || canonicalErr != nil || contractDigest != pins.PlatformSetupContractDigest {
		combined = errors.Join(combined, errors.New("installed Platform Setup Contract digest differs"))
	}
	if installationErr == nil && (pins.Version == "" || pins.ReleaseManifestDigest == "" || pins.PlatformSetupContractDigest == "" || pins.WorkflowCLISHA256 == "" || pins.ReleaseBundledFilesDigest == "" || installation.ReleaseBundledFilesJSON == "") {
		combined = errors.Join(combined, errors.New("Platform Installation lacks durable verified release pins"))
	}
	if installationErr == nil {
		if _, bundleErr := durableReleaseBundle(installation); bundleErr != nil {
			combined = errors.Join(combined, bundleErr)
		}
	}
	if !facts.WorkflowCLI.Verified {
		combined = errors.Join(combined, errors.New("installed Workflow CLI ownership, version, or checksum differs"))
	}
	if !facts.GitHubCredential.Verified {
		combined = errors.Join(combined, errors.New("Control Plane PAT live verification failed"))
	}
	return facts, combined
}

type platformPins struct{ Version, ReleaseManifestDigest, PlatformSetupContractDigest, WorkflowCLISHA256, ReleaseBundledFilesDigest string }

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
	platformReady, err := requirePlatformReadyForOnboarding(context.Background(), database, layout, output)
	if err != nil || !platformReady {
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
		if verifyErr := verifyRecordedAdmissionForSetup(context.Background(), database, layout, client, discovery.Repository); verifyErr == nil {
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
	plan, err := onboarding.Plan(context.Background(), onboarding.PlanOptions{RepositoryPath: *repository, WorkflowHome: layout.Root, Owner: verification.Owner, AuthenticatedLogin: verification.Login, RepositoryName: *repositoryName, Private: &private, Remote: onboardingGitHubRemote{Client: client}, PlatformReleaseDigest: mustPlatformDigest(context.Background(), database), Labels: labels, Policy: client, Publication: client, State: onboardingCurrentState{Client: client, Store: database}, DomainLayout: *domainLayout})
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

func requirePlatformReadyForOnboarding(ctx context.Context, database *store.Store, layout workflowhome.Layout, output io.Writer) (bool, error) {
	if err := verifyPlatformReadyForSetup(ctx, database, layout); err != nil {
		return false, writeSetupResponse(output, setupResponse{Status: "blocked", PlatformReady: false, Blocker: "Platform Ready: " + err.Error()})
	}
	return true, nil
}

func verifySetupReady(ctx context.Context, database *store.Store, layout workflowhome.Layout, client *github.Client, repository string) error {
	platformErr := verifyPlatformReadyForSetup(ctx, database, layout)
	admissionErr := verifyRecordedAdmissionForSetup(ctx, database, layout, client, repository)
	return errors.Join(platformErr, admissionErr)
}

type onboardingGitHubRemote struct{ Client *github.Client }

type onboardingCurrentState struct {
	Client *github.Client
	Store  *store.Store
}

func (d onboardingCurrentState) DiscoverOnboardingState(ctx context.Context, repository, branch, manifestDigest string, labels []onboarding.Label) (onboarding.OnboardingState, error) {
	if d.Client == nil || d.Store == nil {
		return onboarding.OnboardingState{}, errors.New("onboarding state discovery is incomplete")
	}
	result := onboarding.OnboardingState{SatisfiedLabels: map[string]bool{}}
	for _, expected := range labels {
		actual, err := d.Client.Label(ctx, repository, expected.Name)
		if github.IsNotFound(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		result.SatisfiedLabels[expected.Name] = strings.EqualFold(actual.Color, expected.Color) && actual.Description == expected.Description
	}
	var fetchErr error
	_, contractErr := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
		content, err := d.Client.RepositoryFile(ctx, repository, path, branch)
		if err != nil && fetchErr == nil {
			fetchErr = err
		}
		return content, err
	}, repository, branch, manifestDigest)
	if fetchErr != nil && !github.IsNotFound(fetchErr) {
		return result, fetchErr
	}
	result.ContractSatisfied = contractErr == nil
	if admissionValue, err := d.Store.RepositoryAdmission(ctx, repository); err == nil {
		result.AdmissionSatisfied = result.ContractSatisfied && admissionValue.Eligible && admissionValue.ManifestDigestSHA256 == manifestDigest && admissionValue.ContractVersion == "1"
	} else if !errors.Is(err, store.ErrNotFound) {
		return result, err
	}
	return result, nil
}

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
	adapter := setupengine.HostAdapter{Layout: layout, RepositoryPath: plan.Target.RepositoryPath, PlanDigest: digest, OnboardingMergeHeads: map[string]string{}}
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
	engine := setupengine.Engine{Adapter: adapter, SecretInput: &setupengine.SecretInput{Reader: input}, ExpectedResultVerifier: func(ctx context.Context, _ setupcontract.Plan, expected setupcontract.ExpectedResult) error {
		if expected.Kind != "platform_readiness" {
			return nil
		}
		database, openErr := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
		if openErr != nil {
			return openErr
		}
		defer database.Close()
		return verifyPlatformReadyForSetup(ctx, database, layout)
	}}
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
	pins := platformPins{Version: installation.PlatformVersion, ReleaseManifestDigest: installation.ReleaseManifestDigestSHA256, PlatformSetupContractDigest: installation.PlatformSetupContractDigestSHA256, WorkflowCLISHA256: installation.WorkflowCLISHA256, ReleaseBundledFilesDigest: installation.ReleaseBundledFilesDigestSHA256}
	if pins.Version == "" || pins.ReleaseManifestDigest == "" || pins.PlatformSetupContractDigest == "" || pins.WorkflowCLISHA256 == "" || pins.ReleaseBundledFilesDigest == "" || installation.ControlPlanePlanDigestSHA256 == "" {
		return errors.New("Platform Installation lacks durable verified release or Control Plane authorization pins")
	}
	observation := (controlplane.Inspector{}).Inspect(ctx, &record)
	if observation.State != controlplane.StateReady || record.PlatformVersion != installation.PlatformVersion {
		return errors.New("Control Plane process identity, health, version, or approved plan digest differs")
	}
	if record.ApprovedPlanDigestSHA256 != installation.ControlPlanePlanDigestSHA256 {
		return errors.New("Control Plane runtime differs from its durable authorization identity")
	}
	if err := verifyRuntimePlanBinding(ctx, database, record, installation); err != nil {
		return err
	}
	cliVerified, err := (workflowhome.Installation{Layout: layout}).VerifyVersion(pins.Version, pins.WorkflowCLISHA256)
	if err != nil || !cliVerified {
		return errors.Join(errors.New("installed Workflow CLI ownership, version, or checksum differs"), err)
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
	_, contractDigest, err := setupcontract.Canonicalize(contractRaw)
	if err != nil || contractDigest != pins.PlatformSetupContractDigest {
		return errors.Join(errors.New("installed Platform Setup Contract digest differs from approved plan"), err)
	}
	userProfile := os.Getenv("USERPROFILE")
	if userProfile == "" {
		return errors.New("USERPROFILE is required to verify the Workflow Skill Bundle")
	}
	skillsRoot := filepath.Join(userProfile, ".agents", "skills")
	bundledFiles, err := durableReleaseBundle(installation)
	if err != nil {
		return err
	}
	skillFiles := make([]workflowhome.SkillBundleFile, 0)
	for _, file := range bundledFiles {
		if strings.HasPrefix(file.Path, "skills/") {
			skillFiles = append(skillFiles, workflowhome.SkillBundleFile{Path: strings.TrimPrefix(file.Path, "skills/"), SHA256: file.SHA256})
		}
	}
	verified, err := (workflowhome.Installation{Layout: layout}).VerifySkillBundle(workflowhome.SkillBundleSpec{Version: contract.SkillBundle.Version, DestinationRoot: skillsRoot, ManagedSkills: contract.SkillBundle.ManagedSkills, Files: skillFiles})
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

func durableReleaseBundle(installation store.PlatformInstallation) ([]platformrelease.BundledFile, error) {
	canonical, digest, err := setupcontract.Canonicalize([]byte(installation.ReleaseBundledFilesJSON))
	if err != nil || string(canonical) != installation.ReleaseBundledFilesJSON || digest != installation.ReleaseBundledFilesDigestSHA256 {
		return nil, errors.Join(errors.New("durable signed Platform Release bundled-file inventory differs"), err)
	}
	var files []platformrelease.BundledFile
	if json.Unmarshal(canonical, &files) != nil || len(files) == 0 {
		return nil, errors.New("durable signed Platform Release bundled-file inventory is invalid")
	}
	seen, cli := map[string]bool{}, 0
	for _, file := range files {
		if seen[file.Path] || file.Path == "" || filepath.IsAbs(file.Path) || len(file.SHA256) != 64 {
			return nil, errors.New("durable signed Platform Release bundled-file inventory is invalid")
		}
		seen[file.Path] = true
		if file.Path == "bin/workflow.exe" {
			cli++
			if file.SHA256 != installation.WorkflowCLISHA256 {
				return nil, errors.New("durable signed Platform Release Workflow CLI checksum differs")
			}
		}
	}
	if cli != 1 {
		return nil, errors.New("durable signed Platform Release bundled-file inventory lacks one Workflow CLI")
	}
	return files, nil
}

func verifyRuntimePlanBinding(ctx context.Context, database *store.Store, record controlplane.RuntimeRecord, installation store.PlatformInstallation) error {
	archived, err := database.SetupPlanByDigest(ctx, record.ApprovedPlanDigestSHA256)
	if err != nil {
		return errors.Join(errors.New("Control Plane approved plan is not archived"), err)
	}
	plan, canonical, digest, err := setupcontract.ParsePlan([]byte(archived.CanonicalJSON))
	if err != nil || plan.Kind != setupcontract.PlatformBootstrap || digest != archived.DigestSHA256 || string(canonical) != archived.CanonicalJSON {
		return errors.New("Control Plane approved plan archive is invalid")
	}
	for _, effect := range plan.Effects {
		if effect.Kind == "control_plane" && (effect.Action == "start" || effect.Action == "replace") && effect.Parameters["version"] == installation.PlatformVersion && effect.Parameters["release_manifest_digest"] == installation.ReleaseManifestDigestSHA256 && effect.Parameters["platform_setup_contract_digest"] == installation.PlatformSetupContractDigestSHA256 && effect.Parameters["workflow_cli_sha256"] == installation.WorkflowCLISHA256 && effect.Parameters["release_bundled_files_digest"] == installation.ReleaseBundledFilesDigestSHA256 {
			return nil
		}
	}
	return errors.New("Control Plane runtime is not bound to an approved start effect")
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

func verifyRecordedAdmissionReadOnly(ctx context.Context, database *store.Store, layout workflowhome.Layout, client *github.Client, repository string) error {
	value, err := database.RepositoryAdmission(ctx, repository)
	if err != nil || !value.Eligible {
		return errors.Join(errors.New("Repository Admission is not eligible"), err)
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
	return (admission.GitHubVerifier{Client: client, Contract: contract}).Verify(ctx, value)
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
