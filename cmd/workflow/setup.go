package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
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
	Status             string                   `json:"status"`
	PlatformReady      bool                     `json:"platform_ready,omitempty"`
	RepositoryAdmitted bool                     `json:"repository_admitted,omitempty"`
	Blocker            string                   `json:"blocker,omitempty"`
	Result             any                      `json:"result,omitempty"`
	PlanPath           string                   `json:"plan_path,omitempty"`
	DigestSHA256       string                   `json:"digest_sha256,omitempty"`
	Projection         string                   `json:"projection,omitempty"`
	Verification       *setupVerificationReport `json:"verification,omitempty"`
}

type setupVerificationCheck struct {
	Status     string `json:"status"`
	Evidence   string `json:"evidence,omitempty"`
	RepairHint string `json:"repair_hint,omitempty"`
}

type setupVerificationReport struct {
	Credential struct {
		setupVerificationCheck
		Login             string   `json:"login,omitempty"`
		UserID            int64    `json:"user_id,omitempty"`
		Owner             string   `json:"owner,omitempty"`
		Scopes            []string `json:"scopes,omitempty"`
		FingerprintSHA256 string   `json:"fingerprint_sha256,omitempty"`
	} `json:"credential"`
	Discovery struct {
		setupVerificationCheck
		Repository     string `json:"repository,omitempty"`
		RepositoryPath string `json:"repository_path,omitempty"`
		Origin         string `json:"origin,omitempty"`
		DefaultBranch  string `json:"default_branch,omitempty"`
		Head           string `json:"head,omitempty"`
		Published      bool   `json:"published"`
	} `json:"discovery"`
	Admission struct {
		setupVerificationCheck
		Repository                 string `json:"repository,omitempty"`
		OnboardingPlanDigestSHA256 string `json:"onboarding_plan_digest_sha256,omitempty"`
		ContractVersion            string `json:"contract_version,omitempty"`
		ManifestDigestSHA256       string `json:"manifest_digest_sha256,omitempty"`
		Eligible                   bool   `json:"eligible"`
	} `json:"admission"`
	Readiness struct {
		setupVerificationCheck
		PlatformReady   bool `json:"platform_ready"`
		RepositoryReady bool `json:"repository_ready"`
	} `json:"readiness"`
}

func setupVerificationBlocked(err error, repairHint string, secrets ...string) setupVerificationCheck {
	diagnostic := "verification evidence is unavailable"
	if err != nil {
		diagnostic = err.Error()
	}
	for _, secret := range secrets {
		if secret != "" {
			diagnostic = strings.ReplaceAll(diagnostic, secret, "[redacted]")
		}
	}
	return setupVerificationCheck{Status: "blocked", Evidence: diagnostic, RepairHint: repairHint}
}

var (
	verifyPlatformReadyForSetup         = verifyPlatformReadyReadOnly
	verifyPlatformReadyForApply         = verifyPlatformReadyTracked
	verifyPlatformPreconditionsForSetup = verifySatisfiedPlatformComponents
	verifyRecordedAdmissionForSetup     = verifyRecordedAdmissionReadOnly
	resolveCodexAuthForSetup            = codexauth.ResolveChatGPT
	setupInspectionAPIBase              = ""
	setupInspectionHTTPClient           *http.Client
)

func setupCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("setup requires plan, apply, inspect-platform, inspect-platform-installation, or verify")
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
	case "inspect-platform-installation":
		return runSetupInspectPlatformInstallation(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown setup command %q", args[0])
	}
}

func runSetupInspectPlatformInstallation(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup inspect-platform-installation", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	if err := flags.Parse(args); err != nil {
		return err
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	database, err := store.OpenReadOnly(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: err.Error()})
	}
	defer database.Close()
	installation, err := database.PlatformInstallation(context.Background())
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: err.Error()})
	}
	sameHome, err := workflowhome.SameFilesystemPath(installation.WorkflowHome, layout.Root)
	if err != nil || !sameHome {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Installation Workflow Home differs from the inspected target"})
	}
	result := platformInspectionFromInstallation(installation)
	return writeSetupResponse(output, setupResponse{Status: "ready", Result: result})
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
		Exists            bool     `json:"exists"`
		Verified          bool     `json:"verified"`
		Login             string   `json:"login,omitempty"`
		Owner             string   `json:"owner,omitempty"`
		Scopes            []string `json:"scopes,omitempty"`
		FingerprintSHA256 string   `json:"fingerprint_sha256,omitempty"`
		Diagnostic        string   `json:"diagnostic,omitempty"`
	} `json:"github_credential"`
	CodexAuth struct {
		Verified          bool   `json:"verified"`
		Source            string `json:"source,omitempty"`
		FingerprintSHA256 string `json:"fingerprint_sha256,omitempty"`
		Diagnostic        string `json:"diagnostic,omitempty"`
	} `json:"codex_auth"`
}

func platformInspectionFromInstallation(installation store.PlatformInstallation) platformInspection {
	var facts platformInspection
	facts.Platform.InstallationRecorded = true
	facts.Platform.Version = installation.PlatformVersion
	facts.Platform.ReleaseManifestDigest = installation.ReleaseManifestDigestSHA256
	facts.Platform.PlatformSetupContractDigest = installation.PlatformSetupContractDigestSHA256
	facts.Platform.WorkflowCLISHA256 = installation.WorkflowCLISHA256
	facts.Platform.ReleaseBundledFilesJSON = installation.ReleaseBundledFilesJSON
	facts.Platform.ReleaseBundledFilesDigest = installation.ReleaseBundledFilesDigestSHA256
	facts.Platform.ControlPlanePlanDigest = installation.ControlPlanePlanDigestSHA256
	return facts
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
	database, err := store.OpenReadOnly(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: err.Error()})
	}
	if err := database.Close(); err != nil {
		return err
	}
	database, err = store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: err.Error()})
	}
	defer database.Close()
	var cleanupClient *github.Client
	if verification, verificationErr := database.GitHubPATVerification(context.Background()); verificationErr == nil {
		config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
		if token, tokenErr := verifiedClassicPAT(context.Background(), database, config, layout.CredentialFile); tokenErr == nil {
			cleanupClient = github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
		}
	}
	if err := setupengine.DrainPendingCleanupObligations(context.Background(), database, setupengine.HostAdapter{Layout: layout, GitHub: cleanupClient}, time.Now().UTC()); err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "pending Setup cleanup: " + err.Error()})
	}
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
		facts = platformInspectionFromInstallation(installation)
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
		facts.GitHubCredential.Login = verification.Login
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
	if verificationErr == nil {
		facts.GitHubCredential.FingerprintSHA256 = verification.FingerprintSHA256
	}
	authSource, authErr := resolveCodexAuthForSetup(ctx)
	if authErr == nil {
		authContent, readErr := os.ReadFile(authSource)
		if readErr == nil {
			sum := sha256.Sum256(authContent)
			facts.CodexAuth.Verified = true
			facts.CodexAuth.Source = filepath.Clean(authSource)
			facts.CodexAuth.FingerprintSHA256 = hex.EncodeToString(sum[:])
		} else {
			authErr = readErr
		}
	}
	if authErr != nil {
		facts.CodexAuth.Diagnostic = authErr.Error()
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
	if !facts.CodexAuth.Verified {
		combined = errors.Join(combined, errors.New("Codex ChatGPT authentication source verification failed"))
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
	publicationState := flags.String("publication-state", "auto", "published, unpublished, or auto")
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
	if *publicationState != "auto" && *publicationState != "published" && *publicationState != "unpublished" {
		return errors.New("setup plan --publication-state must be published or unpublished")
	}
	if *domainLayout != "single-context" && *domainLayout != "multi-context" {
		return errors.New("setup plan --domain-layout must be single-context or multi-context")
	}
	private := *visibility == "private"
	if *publicationState != "auto" {
		_, originErr := onboarding.ReadLocalOriginURL(context.Background(), *repository)
		originPresent := originErr == nil
		if originErr != nil && !errors.Is(originErr, onboarding.ErrRepositoryOriginAbsent) {
			return originErr
		}
		if (*publicationState == "published") != originPresent {
			return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "current origin differs from the confirmed publication state"})
		}
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(layout.State, "workflow.db")); errors.Is(err, os.ErrNotExist) {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Bootstrap must be completed by the entry skill"})
	}
	database, err := store.OpenReadOnly(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: err.Error()})
	}
	if _, err := database.PlatformInstallation(context.Background()); err != nil {
		database.Close()
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Bootstrap must be completed by the entry skill"})
	}
	if _, err := database.GitHubPATVerification(context.Background()); err != nil {
		database.Close()
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Control Plane GitHub PAT verification is incomplete; rerun the approved Platform Bootstrap Plan"})
	}
	verification, err := database.GitHubPATVerification(context.Background())
	if err != nil {
		database.Close()
		return err
	}
	if err := database.Close(); err != nil {
		return err
	}
	database, err = store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
	token, err := verifiedClassicPAT(context.Background(), database, config, layout.CredentialFile)
	if err != nil {
		return err
	}
	client := github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
	cleanupAdapter := setupengine.HostAdapter{Layout: layout, GitHub: client}
	if err := setupengine.DrainPendingCleanupObligations(context.Background(), database, cleanupAdapter, time.Now().UTC()); err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "pending Setup cleanup: " + err.Error()})
	}
	platformReady, err := requirePlatformReadyForOnboarding(context.Background(), database, layout, output)
	if err != nil || !platformReady {
		return err
	}
	discovery, discoveryErr := onboarding.Discover(context.Background(), *repository, onboardingGitHubRemote{Client: client})
	if discoveryErr == nil && discovery.Repository != "" {
		if verifyErr := verifyRecordedAdmissionForSetup(context.Background(), database, layout, client, discovery.Repository); verifyErr == nil {
			intentErr := verifyConfirmedReadyIntent(context.Background(), database, client, discovery, verification.Owner, *repositoryName, private, *domainLayout)
			if intentErr != nil {
				return writeSetupResponse(output, setupResponse{Status: "blocked", PlatformReady: true, Blocker: "confirmed repository intent: " + intentErr.Error()})
			}
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

func verifyConfirmedReadyIntent(ctx context.Context, database *store.Store, client *github.Client, discovery onboarding.Discovery, owner, repositoryName string, private bool, domainLayout string) error {
	if client == nil || strings.TrimSpace(repositoryName) == "" {
		return errors.New("repository name was not confirmed")
	}
	expectedRepository := owner + "/" + repositoryName
	if !strings.EqualFold(discovery.Repository, expectedRepository) {
		return fmt.Errorf("repository identity is %q, want %q", discovery.Repository, expectedRepository)
	}
	httpsOrigin, err := onboarding.GitHubHTTPSURL(expectedRepository)
	if err != nil {
		return err
	}
	sshOrigin := "git@github.com:" + expectedRepository + ".git"
	if !strings.EqualFold(discovery.Origin, httpsOrigin) && !strings.EqualFold(discovery.Origin, sshOrigin) {
		return errors.New("origin is not the canonical confirmed GitHub HTTPS or SSH URL")
	}
	policy, err := client.DiscoverPolicy(ctx, expectedRepository, discovery.DefaultBranch)
	if err != nil {
		return fmt.Errorf("verify confirmed repository visibility: %w", err)
	}
	if policy.Private != private {
		return errors.New("repository visibility differs from the confirmed intent")
	}
	admissionValue, err := database.RepositoryAdmission(ctx, expectedRepository)
	if err != nil {
		return err
	}
	manifest, err := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
		return client.RepositoryFile(ctx, expectedRepository, path, discovery.DefaultBranch)
	}, expectedRepository, discovery.DefaultBranch, admissionValue.ManifestDigestSHA256)
	if err != nil {
		return fmt.Errorf("verify confirmed Repository Contract: %w", err)
	}
	return validateConfirmedReadyIntent(discovery, policy, manifest, owner, repositoryName, private, domainLayout)
}

func validateConfirmedReadyIntent(discovery onboarding.Discovery, policy onboarding.RepositoryPolicy, manifest repositorycontract.Manifest, owner, repositoryName string, private bool, domainLayout string) error {
	if strings.TrimSpace(repositoryName) == "" {
		return errors.New("repository name was not confirmed")
	}
	expectedRepository := owner + "/" + repositoryName
	if !strings.EqualFold(discovery.Repository, expectedRepository) {
		return fmt.Errorf("repository identity is %q, want %q", discovery.Repository, expectedRepository)
	}
	httpsOrigin, err := onboarding.GitHubHTTPSURL(expectedRepository)
	if err != nil {
		return err
	}
	sshOrigin := "git@github.com:" + expectedRepository + ".git"
	if !strings.EqualFold(discovery.Origin, httpsOrigin) && !strings.EqualFold(discovery.Origin, sshOrigin) {
		return errors.New("origin is not the canonical confirmed GitHub HTTPS or SSH URL")
	}
	if policy.Private != private {
		return errors.New("repository visibility differs from the confirmed intent")
	}
	if manifest.DomainLayout != domainLayout {
		return errors.New("Repository Contract domain layout differs from the confirmed intent")
	}
	return nil
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
	return onboarding.ParseGitHubOrigin(origin)
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
	isOnboarding := plan.Kind == setupcontract.RepositoryOnboarding
	adapter := setupengine.HostAdapter{Layout: layout, RepositoryPath: plan.Target.RepositoryPath, PlanDigest: digest, OnboardingMergeHeads: map[string]string{}, CreatedRepositories: map[string]bool{}, InitialBaselineHeads: map[string]string{}, PublishedHistoryHeads: map[string]string{}, ApprovedGitHubPolicies: map[string]string{}}
	if isOnboarding {
		// Repository-owned Git transport configuration must be rejected before
		// the persistent PAT is read into this process.
		if err := onboarding.ValidateAuthenticatedGitRepository(context.Background(), plan.Target.RepositoryPath); err != nil {
			return err
		}
		for _, precondition := range plan.Preconditions {
			if precondition.Kind == "github_policy" {
				adapter.ApprovedGitHubPolicies[precondition.Subject] = precondition.Expected
			}
		}
	}
	databasePath := filepath.Join(layout.State, "workflow.db")
	if _, statErr := os.Stat(databasePath); statErr == nil {
		database, openErr := store.Open(context.Background(), databasePath)
		if openErr != nil {
			return openErr
		}
		verification, readErr := database.GitHubPATVerification(context.Background())
		if readErr == nil {
			config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
			token, tokenErr := verifiedClassicPAT(context.Background(), database, config, layout.CredentialFile)
			database.Close()
			if tokenErr != nil {
				if isOnboarding {
					return tokenErr
				}
			} else if isOnboarding {
				adapter.GitHub = github.NewClient("", token, nil).WithOnboardingIdentity(verification.Owner, verification.Login, plan.Target.GitHubRepository)
				adapter.CleanupGitHub = github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
				adapter.GitCredential = onboarding.GitCredential{Username: "x-access-token", Token: token}
			} else {
				adapter.GitHub = github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
				adapter.CleanupGitHub = adapter.GitHub
			}
		} else {
			database.Close()
			if isOnboarding {
				return readErr
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else if isOnboarding {
		return errors.New("Repository Onboarding requires an existing Workflow Home database")
	}
	engine := setupengine.Engine{Adapter: &adapter, SecretInput: &setupengine.SecretInput{Reader: input}, PlatformPreconditionVerifier: func(ctx context.Context, plan setupcontract.Plan) error {
		database, openErr := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
		if openErr != nil {
			return openErr
		}
		defer database.Close()
		return verifyPlatformPreconditionsForSetup(ctx, database, layout, adapter, plan)
	}, ExpectedResultVerifier: func(ctx context.Context, currentPlan setupcontract.Plan, expected setupcontract.ExpectedResult) error {
		if expected.Kind != "platform_readiness" {
			return nil
		}
		database, openErr := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
		if openErr != nil {
			return openErr
		}
		defer database.Close()
		tracker := &platformCleanupTracker{database: database, plan: currentPlan, digest: digest, effectID: expected.ID}
		return verifyPlatformReadyForApply(ctx, database, layout, tracker)
	}}
	result, applyErr := engine.Apply(context.Background(), raw, *approved)
	writeErr := writeSetupResponse(output, setupResponse{Status: string(result.Status), Result: result})
	return errors.Join(applyErr, writeErr)
}

func verifySatisfiedPlatformComponents(ctx context.Context, database *store.Store, layout workflowhome.Layout, adapter setupengine.HostAdapter, plan setupcontract.Plan) error {
	planned := map[string]bool{}
	for _, effect := range plan.Effects {
		planned[effect.Kind] = true
	}
	statePreconditions := make([]setupcontract.Precondition, 0, 1)
	for _, precondition := range plan.Preconditions {
		if precondition.Kind == "platform_state" {
			statePreconditions = append(statePreconditions, precondition)
		}
	}
	if len(statePreconditions) != 1 || !strings.EqualFold(filepath.Clean(statePreconditions[0].Subject), filepath.Clean(layout.Root)) {
		return errors.New("approved plan lacks one exact Platform state precondition")
	}
	stateDigest, err := currentPlatformStateDigest(ctx, database, !planned["github_pat"])
	if err != nil {
		return err
	}
	if stateDigest != statePreconditions[0].Expected {
		return errors.New("satisfied PAT or Codex authentication changed after planning")
	}
	installation, err := database.PlatformInstallation(ctx)
	if err != nil {
		if planned["platform_installation"] {
			return nil
		}
		return err
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
	if err != nil || contractDigest != installation.PlatformSetupContractDigestSHA256 {
		return errors.Join(errors.New("installed Platform Setup Contract drifted before mutation"), err)
	}
	pins := map[string]string{"release_manifest_digest": installation.ReleaseManifestDigestSHA256, "platform_setup_contract_digest": installation.PlatformSetupContractDigestSHA256, "workflow_cli_sha256": installation.WorkflowCLISHA256, "release_bundled_files_digest": installation.ReleaseBundledFilesDigestSHA256}
	check := func(effect setupcontract.Effect) error {
		status, evidence, readErr := adapter.Readback(ctx, effect)
		if readErr != nil {
			return readErr
		}
		if status != setupcontract.EffectSatisfied {
			return fmt.Errorf("satisfied %s drifted before mutation: %s", effect.Kind, evidence)
		}
		return nil
	}
	withPins := func(values map[string]string) map[string]string {
		for key, value := range pins {
			values[key] = value
		}
		return values
	}
	if !planned["platform_cli"] {
		if err := check(setupcontract.Effect{ID: "precondition-platform-cli", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: withPins(map[string]string{"version": installation.PlatformVersion, "sha256": installation.WorkflowCLISHA256})}); err != nil {
			return err
		}
	}
	if !planned["workflow_skill_bundle"] {
		userProfile := os.Getenv("USERPROFILE")
		if userProfile == "" {
			return errors.New("USERPROFILE is required to verify the satisfied Workflow Skill Bundle")
		}
		bundled, err := durableReleaseBundle(installation)
		if err != nil {
			return err
		}
		files := make([]workflowhome.SkillBundleFile, 0)
		for _, file := range bundled {
			if strings.HasPrefix(file.Path, "skills/") {
				files = append(files, workflowhome.SkillBundleFile{Path: strings.TrimPrefix(file.Path, "skills/"), SHA256: file.SHA256})
			}
		}
		skillsJSON, _ := json.Marshal(contract.SkillBundle.ManagedSkills)
		filesJSON, _ := json.Marshal(files)
		if err := check(setupcontract.Effect{ID: "precondition-workflow-skill-bundle", Kind: "workflow_skill_bundle", Subject: filepath.Join(userProfile, ".agents", "skills"), Action: "install", Parameters: withPins(map[string]string{"version": contract.SkillBundle.Version, "managed_skills_json": string(skillsJSON), "files_json": string(filesJSON)})}); err != nil {
			return err
		}
	}
	if !planned["docker_desktop"] {
		if err := check(setupcontract.Effect{ID: "precondition-docker-desktop", Kind: "docker_desktop", Subject: "current-host", Action: "repair", Parameters: withPins(map[string]string{"version": contract.Docker.Version, "installer_url": contract.Docker.InstallerURL, "windows_amd64_sha256": contract.Docker.WindowsAMD64SHA256})}); err != nil {
			return err
		}
	}
	if !planned["github_pat"] {
		verification, err := database.GitHubPATVerification(ctx)
		if err != nil || verification.Status != "verified" {
			return errors.Join(errors.New("satisfied GitHub PAT drifted before mutation"), err)
		}
		config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
		token, err := verifiedClassicPAT(ctx, database, config, layout.CredentialFile)
		if err != nil {
			return err
		}
		if result := (doctor.GitHubPATCheck{Pin: config.GitHub.Credential, Token: token, Verification: verification}).Run(ctx); result.Status != doctor.Pass {
			return errors.New(result.Summary)
		}
	}
	if !planned["control_plane"] && !planned["platform_installation"] {
		record, err := controlplane.ReadRuntimeRecord(layout)
		if err != nil {
			return err
		}
		if observation := (controlplane.Inspector{}).Inspect(ctx, &record); observation.State != controlplane.StateReady || record.PlatformVersion != installation.PlatformVersion || record.ApprovedPlanDigestSHA256 != installation.ControlPlanePlanDigestSHA256 {
			return errors.New("satisfied Control Plane drifted before mutation")
		}
		if err := verifyRuntimePlanBinding(ctx, database, record, installation); err != nil {
			return err
		}
	}
	if _, err := codexauth.ResolveChatGPT(ctx); err != nil {
		return fmt.Errorf("satisfied Codex authentication drifted before mutation: %w", err)
	}
	return nil
}

type platformStateSnapshot struct {
	CodexAuth struct {
		Source            string `json:"source"`
		FingerprintSHA256 string `json:"fingerprint_sha256"`
	} `json:"codex_auth"`
	GitHubPAT *struct {
		FingerprintSHA256 string   `json:"fingerprint_sha256"`
		Owner             string   `json:"owner"`
		Scopes            []string `json:"scopes"`
	} `json:"github_pat,omitempty"`
}

func currentPlatformStateDigest(ctx context.Context, database *store.Store, includePAT bool) (string, error) {
	source, err := resolveCodexAuthForSetup(ctx)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	var snapshot platformStateSnapshot
	sum := sha256.Sum256(content)
	canonicalSource, canonicalErr := workflowhome.CanonicalFilesystemPath(source)
	if canonicalErr != nil {
		return "", canonicalErr
	}
	snapshot.CodexAuth.Source = canonicalSource
	snapshot.CodexAuth.FingerprintSHA256 = hex.EncodeToString(sum[:])
	if includePAT {
		verification, err := database.GitHubPATVerification(ctx)
		if err != nil {
			return "", err
		}
		value := struct {
			FingerprintSHA256 string   `json:"fingerprint_sha256"`
			Owner             string   `json:"owner"`
			Scopes            []string `json:"scopes"`
		}{FingerprintSHA256: verification.FingerprintSHA256, Owner: verification.Owner, Scopes: append([]string(nil), verification.Scopes...)}
		snapshot.GitHubPAT = &value
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	_, digest, err := setupcontract.Canonicalize(raw)
	return digest, err
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
	ctx := context.Background()
	verification, credentialErr := database.GitHubPATVerification(ctx)
	token := ""
	var tokenErr error
	var cleanupClient *github.Client
	if credentialErr == nil {
		config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
		token, tokenErr = verifiedClassicPAT(ctx, database, config, layout.CredentialFile)
		if tokenErr == nil {
			cleanupClient = github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
		}
	}
	cleanupErr := setupengine.DrainPendingCleanupObligations(ctx, database, setupengine.HostAdapter{Layout: layout, GitHub: cleanupClient}, time.Now().UTC())
	tracker, trackerErr := durablePlatformCleanupTracker(ctx, database)
	platformErr := errors.Join(credentialErr, tokenErr, cleanupErr, trackerErr)
	if platformErr == nil {
		platformErr = verifyPlatformReadyTracked(ctx, database, layout, tracker)
	}
	platformReady := platformErr == nil
	report := &setupVerificationReport{}
	report.Readiness.PlatformReady = platformReady
	if platformErr == nil {
		report.Readiness.setupVerificationCheck = setupVerificationCheck{Status: "verified", Evidence: "Platform Installation, Control Plane, Docker Worker, Workflow Skill Bundle, and Codex session are ready"}
	} else {
		report.Readiness.setupVerificationCheck = setupVerificationBlocked(platformErr, "rerun the approved Platform Bootstrap repair plan")
	}
	report.Credential.Login, report.Credential.UserID, report.Credential.Owner = verification.Login, verification.UserID, verification.Owner
	report.Credential.Scopes = append([]string(nil), verification.Scopes...)
	report.Credential.FingerprintSHA256 = verification.FingerprintSHA256
	repositoryReady := false
	if credentialErr == nil {
		config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
		if token == "" && tokenErr == nil {
			token, tokenErr = verifiedClassicPAT(context.Background(), database, config, layout.CredentialFile)
		}
		if tokenErr == nil {
			report.Credential.setupVerificationCheck = setupVerificationCheck{Status: "verified", Evidence: "persisted PAT fingerprint, login, user ID, owner, scopes, and credential path match the live credential"}
			client := github.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
			if discovery, discoveryErr := onboarding.Discover(context.Background(), *repository, onboardingGitHubRemote{Client: client}); discoveryErr == nil && discovery.Repository != "" {
				report.Discovery.Repository, report.Discovery.RepositoryPath, report.Discovery.Origin = discovery.Repository, discovery.Root, discovery.Origin
				report.Discovery.DefaultBranch, report.Discovery.Head, report.Discovery.Published = discovery.DefaultBranch, discovery.Head, discovery.Published
				report.Discovery.setupVerificationCheck = setupVerificationCheck{Status: "verified", Evidence: "local root, canonical GitHub origin, default branch, and exact HEAD match read-only discovery"}
				admissionErr := verifyRecordedAdmission(context.Background(), database, layout, client, discovery.Repository)
				repositoryReady = admissionErr == nil
				if admission, readErr := database.RepositoryAdmission(context.Background(), discovery.Repository); readErr == nil {
					report.Admission.Repository, report.Admission.OnboardingPlanDigestSHA256 = admission.Repository, admission.OnboardingPlanDigestSHA256
					report.Admission.ContractVersion, report.Admission.ManifestDigestSHA256, report.Admission.Eligible = admission.ContractVersion, admission.ManifestDigestSHA256, admission.Eligible
				}
				if admissionErr == nil {
					report.Admission.setupVerificationCheck = setupVerificationCheck{Status: "verified", Evidence: "eligible admission matches the exact Onboarding Plan and Repository Contract Manifest digests"}
				} else {
					report.Admission.setupVerificationCheck = setupVerificationBlocked(admissionErr, "rerun Repository Onboarding to forward-repair admission and managed contract drift", token)
				}
			} else {
				report.Discovery.setupVerificationCheck = setupVerificationBlocked(discoveryErr, "restore the confirmed canonical origin, default branch, and approved repository HEAD", token)
			}
		} else {
			report.Credential.setupVerificationCheck = setupVerificationBlocked(tokenErr, "rerun the approved credential repair with the confirmed owner and classic PAT")
		}
	} else {
		report.Credential.setupVerificationCheck = setupVerificationBlocked(credentialErr, "rerun the approved credential repair with the confirmed owner and classic PAT")
	}
	if report.Discovery.Status == "" {
		report.Discovery.setupVerificationCheck = setupVerificationBlocked(errors.New("credential verification did not authorize repository discovery"), "repair the Control Plane GitHub credential, then rerun setup verify")
	}
	if report.Admission.Status == "" {
		report.Admission.setupVerificationCheck = setupVerificationBlocked(errors.New("exact repository discovery did not authorize admission verification"), "repair repository discovery, then rerun Repository Onboarding")
	}
	report.Readiness.RepositoryReady = repositoryReady
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
		blocker = setupVerificationBlocked(errors.New(blocker), "follow the stage-specific repair hints", token).Evidence
	}
	if platformReady && repositoryReady {
		report.Readiness.setupVerificationCheck = setupVerificationCheck{Status: "verified", Evidence: "Platform Ready and exact Repository Admission verification both passed"}
	}
	return writeSetupResponse(output, setupResponse{Status: status, PlatformReady: platformReady, RepositoryAdmitted: repositoryReady, Blocker: blocker, Verification: report})
}

func verifyPlatformReady(ctx context.Context, database *store.Store, layout workflowhome.Layout) error {
	return verifyPlatformReadyReadOnly(ctx, database, layout)
}

func verifyPlatformReadyReadOnly(ctx context.Context, database *store.Store, layout workflowhome.Layout) error {
	return verifyPlatformReadyMode(ctx, database, layout, nil, false)
}

type platformCleanupTracker struct {
	database         *store.Store
	plan             setupcontract.Plan
	digest, effectID string
}

func durablePlatformCleanupTracker(ctx context.Context, database *store.Store) (*platformCleanupTracker, error) {
	installation, err := database.PlatformInstallation(ctx)
	if err != nil || installation.ControlPlanePlanDigestSHA256 == "" {
		return nil, errors.Join(errors.New("Platform cleanup authorization is unavailable"), err)
	}
	archived, err := database.SetupPlanByDigest(ctx, installation.ControlPlanePlanDigestSHA256)
	if err != nil {
		return nil, errors.Join(errors.New("Platform cleanup authorization plan is unavailable"), err)
	}
	plan, canonical, digest, err := setupcontract.ParsePlan([]byte(archived.CanonicalJSON))
	if err != nil || digest != archived.DigestSHA256 || string(canonical) != archived.CanonicalJSON || plan.PlanID != archived.PlanID || plan.Kind != setupcontract.PlatformBootstrap {
		return nil, errors.New("Platform cleanup authorization plan is invalid")
	}
	return &platformCleanupTracker{database: database, plan: plan, digest: digest, effectID: "platform-verify"}, nil
}

func (t *platformCleanupTracker) begin(kind, id, resource string) error {
	if t == nil {
		return nil
	}
	return t.database.RecordSetupCleanupObligation(context.Background(), store.SetupCleanupObligation{
		PlanID: t.plan.PlanID, PlanDigestSHA256: t.digest, EffectID: t.effectID,
		ObligationID: t.effectID + ":" + id, Kind: kind, Resource: resource,
		Status: store.CleanupPending, UpdatedAt: time.Now().UTC(),
	})
}

func (t *platformCleanupTracker) complete(id string) error {
	if t == nil {
		return nil
	}
	return t.database.CompleteSetupCleanupObligation(context.Background(), t.plan.PlanID, t.effectID+":"+id, time.Now().UTC())
}

func verifyPlatformReadyTracked(ctx context.Context, database *store.Store, layout workflowhome.Layout, tracker *platformCleanupTracker) error {
	if tracker == nil {
		return errors.New("tracked Platform Ready verification requires durable cleanup tracking")
	}
	return verifyPlatformReadyMode(ctx, database, layout, tracker, true)
}

func verifyPlatformReadyMode(ctx context.Context, database *store.Store, layout workflowhome.Layout, tracker *platformCleanupTracker, entityProbes bool) error {
	installation, err := database.PlatformInstallation(ctx)
	if err != nil {
		return err
	}
	verification, err := database.GitHubPATVerification(ctx)
	if err != nil || verification.Status != "verified" {
		return errors.Join(errors.New("Control Plane PAT verification is unavailable"), err)
	}
	config := doctor.Config{SchemaVersion: 6, GitHub: doctor.GitHubPin{Credential: doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: verification.Owner, PlaintextRelativePath: `state\credentials\github.pat`}}}
	token, err := verifiedClassicPAT(ctx, database, config, layout.CredentialFile)
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
	if err := verifyDockerDesktopStatus(ctx, contract.Docker, hostsetup.WindowsDockerDesktopHost{}); err != nil {
		return err
	}
	if !entityProbes {
		return setupengine.RequireNoPendingCleanupObligations(ctx, database)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	dockerVerifier := hostsetup.DockerWorkerVerifier{ProbeID: digestPrefix(tracker), BeginCleanup: cleanupBegin(tracker), CompleteCleanup: cleanupComplete(tracker)}
	if err := dockerVerifier.Verify(probeCtx, contract.Worker.Image, layout.State, layout.Workspaces); err != nil {
		return err
	}
	authFile, err := codexauth.ResolveChatGPT(probeCtx)
	if err != nil {
		return err
	}
	result := (doctor.WorkerCodexSessionCheck{Executor: doctor.OSExecutor{}, Image: contract.Worker.Image, AuthFile: authFile, ProbeID: digestPrefix(tracker), BeginCleanup: cleanupBegin(tracker), CompleteCleanup: cleanupComplete(tracker)}).Run(probeCtx)
	if result.Status != doctor.Pass {
		return errors.New(result.Summary)
	}
	return setupengine.RequireNoPendingCleanupObligations(ctx, database)
}

func verifyDockerDesktopStatus(ctx context.Context, contract platformrelease.DockerDependency, host hostsetup.DockerDesktopHost) error {
	if host == nil {
		return errors.New("Docker Desktop status reader is required")
	}
	version, err := host.InstalledVersion(ctx)
	if err != nil || version != contract.Version {
		return errors.Join(fmt.Errorf("Docker Desktop version %q differs from approved %q", version, contract.Version), err)
	}
	if err := host.EngineReady(ctx); err != nil {
		return fmt.Errorf("Docker Desktop Linux amd64 engine is not ready: %w", err)
	}
	return nil
}

func digestPrefix(tracker *platformCleanupTracker) string {
	if tracker == nil || len(tracker.digest) < 12 {
		return ""
	}
	return tracker.digest[:12]
}
func cleanupBegin(tracker *platformCleanupTracker) func(string, string, string) error {
	if tracker == nil {
		return nil
	}
	return tracker.begin
}
func cleanupComplete(tracker *platformCleanupTracker) func(string) error {
	if tracker == nil {
		return nil
	}
	return tracker.complete
}

func durableReleaseBundle(installation store.PlatformInstallation) ([]platformrelease.BundledFile, error) {
	canonical, digest, err := setupcontract.Canonicalize([]byte(installation.ReleaseBundledFilesJSON))
	if err != nil || string(canonical) != installation.ReleaseBundledFilesJSON || digest != installation.ReleaseBundledFilesDigestSHA256 {
		return nil, errors.Join(errors.New("durable verified Platform Release bundled-file inventory differs"), err)
	}
	var files []platformrelease.BundledFile
	if json.Unmarshal(canonical, &files) != nil || len(files) == 0 {
		return nil, errors.New("durable verified Platform Release bundled-file inventory is invalid")
	}
	seen, cli := map[string]bool{}, 0
	for _, file := range files {
		if seen[file.Path] || file.Path == "" || filepath.IsAbs(file.Path) || len(file.SHA256) != 64 {
			return nil, errors.New("durable verified Platform Release bundled-file inventory is invalid")
		}
		seen[file.Path] = true
		if file.Path == "bin/workflow.exe" {
			cli++
			if file.SHA256 != installation.WorkflowCLISHA256 {
				return nil, errors.New("durable verified Platform Release Workflow CLI checksum differs")
			}
		}
	}
	if cli != 1 {
		return nil, errors.New("durable verified Platform Release bundled-file inventory lacks one Workflow CLI")
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
