package setup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/hostsetup"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type HostAdapter struct {
	Layout                     workflowhome.Layout
	Executable                 string
	SkillBundleSource          string
	PersistUserPATH            func(string) error
	GitHub                     *github.Client
	GitCredential              onboarding.GitCredential
	RepositoryPath             string
	PlanDigest                 string
	TemporaryRoot              string
	StartControlPlane          func(context.Context, controlplane.StartOptions) (controlplane.RuntimeRecord, error)
	InspectControlPlane        func(context.Context, *controlplane.RuntimeRecord) controlplane.Observation
	DockerDesktopHost          hostsetup.DockerDesktopHost
	CurrentUserPATHReconciled  func(string) (bool, error)
	OnboardingMergeHeads       map[string]string
	CreatedRepositories        map[string]bool
	InitialBaselineHeads       map[string]string
	PublishedHistoryHeads      map[string]string
	OnboardingCheckDiagnostics map[string][]string
	CleanupOnboardingWorkspace func(onboarding.GitWorkspace) error
}

const onboardingMergeHeadEvidence = "onboarding_merge_head="

type hostIdentityPrecondition struct {
	UserID              string `json:"user_id"`
	Username            string `json:"username"`
	WorkflowHome        string `json:"workflow_home"`
	WorkflowHomeOwnerID string `json:"workflow_home_owner_id"`
}

const repositoryCreatedEvidence = "repository_created="
const initialBaselineHeadEvidence = "initial_baseline_head="
const publishedHistoryHeadEvidence = "published_history_head="

func (a HostAdapter) RestoreEffectResults(results []setupcontract.EffectResult) error {
	for _, result := range results {
		if index := strings.Index(result.Evidence, repositoryCreatedEvidence); index >= 0 && a.CreatedRepositories != nil {
			repository := strings.TrimSpace(result.Evidence[index+len(repositoryCreatedEvidence):])
			if !strings.Contains(repository, "/") {
				return errors.New("persisted repository-creation evidence is invalid")
			}
			a.CreatedRepositories[repository] = true
		}
		if index := strings.Index(result.Evidence, initialBaselineHeadEvidence); index >= 0 && a.InitialBaselineHeads != nil {
			head := strings.TrimSpace(result.Evidence[index+len(initialBaselineHeadEvidence):])
			if !fullSetupCommitID(head) {
				return errors.New("persisted Initial Repository Baseline HEAD is invalid")
			}
			a.InitialBaselineHeads[result.EffectID] = head
		}
		if index := strings.Index(result.Evidence, publishedHistoryHeadEvidence); index >= 0 && a.PublishedHistoryHeads != nil {
			head := strings.TrimSpace(result.Evidence[index+len(publishedHistoryHeadEvidence):])
			if !fullSetupCommitID(head) {
				return errors.New("persisted published-history HEAD is invalid")
			}
			a.PublishedHistoryHeads[result.EffectID] = head
		}
		if a.OnboardingMergeHeads == nil {
			continue
		}
		index := strings.Index(result.Evidence, onboardingMergeHeadEvidence)
		if index < 0 {
			continue
		}
		value := result.Evidence[index+len(onboardingMergeHeadEvidence):]
		if len(value) < 40 {
			return errors.New("persisted Onboarding Pull Request merge HEAD is invalid")
		}
		head := value[:40]
		if !fullSetupCommitID(head) {
			return errors.New("persisted Onboarding Pull Request merge HEAD is invalid")
		}
		if existing := a.OnboardingMergeHeads[result.EffectID]; existing != "" && existing != head {
			return errors.New("persisted Onboarding Pull Request merge HEAD conflicts across retries")
		}
		a.OnboardingMergeHeads[result.EffectID] = head
	}
	return nil
}

func (a HostAdapter) Readback(ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	if err := setupcontract.ValidateEffectForExecution(effect); err != nil {
		return setupcontract.EffectFailed, "", err
	}
	handler, ok := effectHandlers[effect.Kind]
	if !ok {
		return setupcontract.EffectFailed, "", fmt.Errorf("unsupported Setup effect kind %q", effect.Kind)
	}
	return handler.readback(a, ctx, effect)
}

func hostReadbackHandlers() map[string]effectReadback {
	return map[string]effectReadback{
		"github_pat": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			_, err := credential.NewFileStore(a.Layout.CredentialFile).Get(ctx, credential.GatewayTarget)
			if errors.Is(err, credential.ErrNotFound) {
				return setupcontract.EffectRequired, "credential file is absent", nil
			}
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			return setupcontract.EffectSatisfied, "credential file exists", nil
		},
		"platform_cli": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			verified, err := (workflowhome.Installation{Layout: a.Layout}).VerifyVersion(effect.Parameters["version"], effect.Parameters["sha256"])
			if err != nil {
				if strings.Contains(err.Error(), "not owned") {
					return setupcontract.EffectConflicting, err.Error(), nil
				}
				return setupcontract.EffectFailed, "", err
			}
			if !verified {
				return setupcontract.EffectRequired, "platform CLI version or checksum differs", nil
			}
			pathCheck := a.CurrentUserPATHReconciled
			if pathCheck == nil {
				pathCheck = workflowhome.CurrentUserPathIsReconciled
			}
			reconciled, err := pathCheck(a.Layout.Bin)
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if !reconciled {
				return setupcontract.EffectRequired, "platform CLI exists but current-user PATH needs reconciliation", nil
			}
			return setupcontract.EffectSatisfied, "platform CLI ownership, version, checksum, and PATH match", nil
		},
		"workflow_skill_bundle": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			spec, err := skillBundleSpec(effect)
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			verified, err := (workflowhome.Installation{Layout: a.Layout}).VerifySkillBundle(spec)
			if err != nil {
				if strings.Contains(err.Error(), "not owned") {
					return setupcontract.EffectConflicting, err.Error(), nil
				}
				return setupcontract.EffectFailed, "", err
			}
			if !verified {
				return setupcontract.EffectRequired, "Workflow Skill Bundle is missing or differs from the exact release", nil
			}
			return setupcontract.EffectSatisfied, "Workflow Skill Bundle version and digests match", nil
		},
		"docker_desktop": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			host := a.DockerDesktopHost
			if host == nil {
				host = hostsetup.WindowsDockerDesktopHost{}
			}
			version, err := host.InstalledVersion(ctx)
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if version != effect.Parameters["version"] {
				return setupcontract.EffectRequired, fmt.Sprintf("Docker Desktop version %q differs from approved %q", version, effect.Parameters["version"]), nil
			}
			if err := host.EngineReady(ctx); err != nil {
				return setupcontract.EffectRequired, err.Error(), nil
			}
			return setupcontract.EffectSatisfied, "exact Docker Desktop version and Linux amd64 engine are ready", nil
		},
		"control_plane": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			record, err := controlplane.ReadRuntimeRecord(a.Layout)
			if errors.Is(err, os.ErrNotExist) {
				return setupcontract.EffectRequired, "Control Plane is stopped", nil
			}
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			inspect := a.InspectControlPlane
			var observation controlplane.Observation
			if inspect != nil {
				observation = inspect(ctx, &record)
			} else {
				observation = (controlplane.Inspector{}).Inspect(ctx, &record)
			}
			if observation.State == controlplane.StateStale {
				return setupcontract.EffectRequired, observation.Diagnostic, nil
			}
			if observation.State != controlplane.StateReady {
				return setupcontract.EffectConflicting, observation.Diagnostic, nil
			}
			if record.PlatformVersion != effect.Parameters["version"] || record.ApprovedPlanDigestSHA256 != a.PlanDigest {
				if effect.Action == "replace" {
					return setupcontract.EffectRequired, "approved Control Plane replacement is required", nil
				}
				return setupcontract.EffectConflicting, "a different approved Control Plane instance is running", nil
			}
			return setupcontract.EffectSatisfied, "Control Plane process identity and health are verified", nil
		},
		"create_repository": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			repository, err := a.GitHub.RepositoryForOnboarding(ctx, effect.Subject)
			if github.IsNotFound(err) {
				return setupcontract.EffectRequired, "GitHub repository is absent", nil
			}
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			wantPrivate := effect.Parameters["private"] == "true"
			if repository.Private != wantPrivate {
				return setupcontract.EffectConflicting, "existing GitHub repository visibility differs from approved plan", nil
			}
			evidence := "GitHub repository exists with approved identity and visibility"
			if a.CreatedRepositories[effect.Subject] {
				evidence += "; " + repositoryCreatedEvidence + effect.Subject
			}
			return setupcontract.EffectSatisfied, evidence, nil
		},
		"initial_baseline": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			head, err := gitCommandOutput(ctx, effect.Subject, "rev-parse", "--verify", "HEAD")
			if err != nil {
				return setupcontract.EffectRequired, "Initial Repository Baseline is absent", nil
			}
			message, err := gitCommandOutput(ctx, effect.Subject, "log", "-1", "--format=%B")
			if err != nil || !strings.Contains(message, "Setup-Plan-SHA256: "+a.PlanDigest) {
				return setupcontract.EffectConflicting, "existing baseline is not bound to the approved plan", nil
			}
			var approved []onboarding.BaselineFile
			if json.Unmarshal([]byte(effect.Parameters["files_json"]), &approved) != nil {
				return setupcontract.EffectFailed, "", errors.New("Initial Repository Baseline file contract is invalid")
			}
			if err := onboarding.VerifyInitialBaseline(ctx, effect.Subject, head, approved); err != nil {
				return setupcontract.EffectConflicting, "Initial Repository Baseline tree differs", nil
			}
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			remote, err := a.GitHub.DefaultBranchHead(ctx, effect.Parameters["repository"])
			if github.IsNotFound(err) || err == nil && (remote.Name != effect.Parameters["branch"] || remote.Head != head) {
				return setupcontract.EffectRequired, "Initial Repository Baseline is not published", nil
			}
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			return setupcontract.EffectSatisfied, "approved Initial Repository Baseline is published; " + initialBaselineHeadEvidence + head, nil
		},
		"publish_history": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			revision, err := a.GitHub.DefaultBranchHead(ctx, effect.Subject)
			if github.IsNotFound(err) || github.IsConflict(err) && a.CreatedRepositories[effect.Subject] {
				return setupcontract.EffectRequired, "committed history is not published", nil
			}
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if revision.Head != effect.Parameters["head"] {
				return setupcontract.EffectConflicting, "published default branch differs from approved committed history", nil
			}
			return setupcontract.EffectSatisfied, "approved committed history is published; " + publishedHistoryHeadEvidence + revision.Head, nil
		},
		"github_label": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			repository := strings.SplitN(effect.Subject, "#", 2)[0]
			label, err := a.GitHub.Label(ctx, repository, effect.Parameters["name"])
			if github.IsNotFound(err) {
				return setupcontract.EffectRequired, "managed label is absent", nil
			}
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if !strings.EqualFold(label.Color, effect.Parameters["color"]) || label.Description != effect.Parameters["description"] {
				return setupcontract.EffectRequired, "managed label metadata differs", nil
			}
			return setupcontract.EffectSatisfied, "managed label matches", nil
		},
		"repository_features": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			policy, err := a.GitHub.DiscoverPolicy(ctx, effect.Subject, "")
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if !policy.HasIssues || !policy.ActionsEnabled || policy.ActionsAllowed != effect.Parameters["allowed_actions"] {
				return setupcontract.EffectRequired, "GitHub Issues or Actions is disabled", nil
			}
			return setupcontract.EffectSatisfied, "GitHub Issues and Actions are enabled", nil
		},
		"repository_contract_pr": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			if a.OnboardingMergeHeads == nil {
				return setupcontract.EffectFailed, "", errors.New("durable Onboarding Pull Request merge-HEAD binding is required")
			}
			var fetchErr error
			_, err := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
				content, fileErr := a.GitHub.RepositoryFile(ctx, effect.Subject, path, effect.Parameters["base_branch"])
				if fileErr != nil && fetchErr == nil {
					fetchErr = fileErr
				}
				return content, fileErr
			}, effect.Subject, effect.Parameters["base_branch"], effect.Parameters["manifest_digest"])
			if fetchErr != nil && !github.IsNotFound(fetchErr) {
				return setupcontract.EffectFailed, "", fetchErr
			}
			if err != nil {
				return setupcontract.EffectRequired, err.Error(), nil
			}
			if len(a.PlanDigest) < 12 {
				return setupcontract.EffectFailed, "", errors.New("approved plan digest is required to read back the Onboarding Pull Request")
			}
			owner := strings.SplitN(effect.Subject, "/", 2)[0]
			branch := "workflow/onboarding-" + a.PlanDigest[:12]
			pull, found, err := a.GitHub.FindOnboardingPullRequest(ctx, effect.Subject, owner, branch, effect.Parameters["base_branch"])
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			body := "Approved Setup Plan SHA-256: " + a.PlanDigest
			if !found || pull.Body != body || pull.MergedAt == "" || !fullSetupCommitID(pull.MergeCommitSHA) {
				return setupcontract.EffectConflicting, "merged Onboarding Pull Request is not bound to the approved plan", nil
			}
			if persisted := a.OnboardingMergeHeads[effect.ID]; persisted != "" && persisted != pull.MergeCommitSHA {
				return setupcontract.EffectConflicting, "merged Onboarding Pull Request HEAD differs from persisted Setup evidence", nil
			}
			a.OnboardingMergeHeads[effect.ID] = pull.MergeCommitSHA
			remote, err := a.GitHub.DefaultBranchHead(ctx, effect.Subject)
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if remote.Name != effect.Parameters["base_branch"] || remote.Head != pull.MergeCommitSHA {
				return setupcontract.EffectConflicting, onboardingMergeHeadEvidence + pull.MergeCommitSHA + "; default branch advanced after the approved Onboarding Pull Request merge", nil
			}
			return setupcontract.EffectSatisfied, onboardingMergeHeadEvidence + pull.MergeCommitSHA, nil
		},
		"repository_admission": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			manifest, err := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
				return a.GitHub.RepositoryFile(ctx, effect.Subject, path, effect.Parameters["default_branch"])
			}, effect.Subject, effect.Parameters["default_branch"], effect.Parameters["manifest_digest"])
			if err != nil {
				return setupcontract.EffectRequired, err.Error(), nil
			}
			if manifest.ContractVersion != effect.Parameters["contract_version"] {
				return setupcontract.EffectRequired, "Repository Contract version differs", nil
			}
			var labels []onboarding.Label
			if err := json.Unmarshal([]byte(effect.Parameters["labels_json"]), &labels); err != nil {
				return setupcontract.EffectFailed, "", err
			}
			for _, expected := range labels {
				actual, err := a.GitHub.Label(ctx, effect.Subject, expected.Name)
				if err != nil || !strings.EqualFold(actual.Color, expected.Color) || actual.Description != expected.Description {
					return setupcontract.EffectRequired, "managed GitHub label differs: " + expected.Name, nil
				}
			}
			policy, err := a.GitHub.DiscoverPolicy(ctx, effect.Subject, effect.Parameters["default_branch"])
			if err != nil || !policy.HasIssues || !policy.ActionsEnabled || effect.Parameters["actions_allowed"] != "" && policy.ActionsAllowed != effect.Parameters["actions_allowed"] {
				return setupcontract.EffectRequired, "GitHub Issues or Actions is not available", nil
			}
			return setupcontract.EffectSatisfied, "Repository Contract and managed GitHub resources are verified", nil
		},
		"local_fast_forward": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			if a.GitHub == nil {
				return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
			}
			mergeEvidenceID := effect.Parameters["merge_head_effect_id"]
			mergeHead := a.OnboardingMergeHeads[mergeEvidenceID]
			if mergeEvidenceID == "" || !fullSetupCommitID(mergeHead) {
				return setupcontract.EffectFailed, "", errors.New("persisted Onboarding Pull Request merge HEAD is required for local synchronization")
			}
			remote, err := a.GitHub.DefaultBranchHead(ctx, effect.Parameters["repository"])
			if err != nil {
				return setupcontract.EffectFailed, "", err
			}
			if remote.Name != effect.Parameters["branch"] || remote.Head != mergeHead {
				return setupcontract.EffectConflicting, onboardingMergeHeadEvidence + mergeHead + "; GitHub default branch advanced after the approved onboarding merge", nil
			}
			branch, branchErr := onboardingGitBranch(ctx, effect.Subject)
			if branchErr != nil || branch != effect.Parameters["branch"] {
				return setupcontract.EffectConflicting, "checked-out branch differs from the approved default branch", nil
			}
			local, err := gitCommandOutput(ctx, effect.Subject, "rev-parse", "--verify", effect.Parameters["branch"])
			if err == nil && local == mergeHead {
				return setupcontract.EffectSatisfied, "local default branch matches admitted GitHub branch", nil
			}
			expected := effect.Parameters["pre_merge_head"]
			if expected != "" && local != expected {
				return setupcontract.EffectConflicting, "local pre-merge HEAD differs from the approved plan", nil
			}
			if expected == "" {
				message, messageErr := gitCommandOutput(ctx, effect.Subject, "log", "-1", "--format=%B")
				if messageErr != nil || !strings.Contains(message, "Setup-Plan-SHA256: "+a.PlanDigest) {
					return setupcontract.EffectConflicting, "local pre-merge baseline is not bound to the approved plan", nil
				}
			}
			return setupcontract.EffectRequired, "local default branch requires a safe fast-forward", nil
		},
		"platform_installation": func(HostAdapter, context.Context, setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
			return setupcontract.EffectFailed, "", errors.New("Platform Installation effects require Engine durable state")
		},
	}
}

func (a HostAdapter) Apply(ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
	if err := setupcontract.ValidateEffectForExecution(effect); err != nil {
		return err
	}
	handler, ok := effectHandlers[effect.Kind]
	if !ok {
		return fmt.Errorf("unsupported Setup effect kind %q", effect.Kind)
	}
	return handler.apply(a, ctx, effect, input)
}

func hostApplyHandlers() map[string]effectApply {
	return map[string]effectApply{
		"github_pat": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			secret, err := input.Read()
			if err != nil {
				return err
			}
			if err := workflowhome.SecureCredentialPath(a.Layout.CredentialFile, true); err != nil {
				return err
			}
			token := strings.TrimSpace(string(secret))
			if token == "" || strings.ContainsAny(token, " \t\r\n") {
				return errors.New("classic PAT must be one non-empty token")
			}
			return credential.NewFileStore(a.Layout.CredentialFile).Set(ctx, credential.GatewayTarget, token)
		},
		"platform_cli": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			source := a.Executable
			if source == "" {
				source, _ = os.Executable()
			}
			version := effect.Parameters["version"]
			expectedSHA256 := effect.Parameters["sha256"]
			if version == "" || len(expectedSHA256) != 64 {
				return errors.New("platform CLI effect requires an exact version and checksum")
			}
			if err := (workflowhome.Installation{Layout: a.Layout}).InstallVersion(version, source, expectedSHA256); err != nil {
				return err
			}
			persist := a.PersistUserPATH
			if persist == nil {
				persist = workflowhome.PersistCurrentUserPath
			}
			return persist(a.Layout.Bin)
		},
		"workflow_skill_bundle": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			spec, err := skillBundleSpec(effect)
			if err != nil {
				return err
			}
			source := a.SkillBundleSource
			if source == "" {
				executable := a.Executable
				if executable == "" {
					executable, err = os.Executable()
					if err != nil {
						return err
					}
				}
				source = filepath.Join(filepath.Dir(filepath.Dir(executable)), "skills")
			}
			return (workflowhome.Installation{Layout: a.Layout}).InstallSkillBundle(source, spec)
		},
		"docker_desktop": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			contract := hostsetup.DockerDesktopContract{Version: effect.Parameters["version"], InstallerURL: effect.Parameters["installer_url"], WindowsAMD64SHA256: effect.Parameters["windows_amd64_sha256"]}
			host := a.DockerDesktopHost
			if host == nil {
				host = hostsetup.WindowsDockerDesktopHost{}
			}
			return hostsetup.EnsureDockerDesktop(ctx, contract, host, filepath.Join(a.Layout.Workspaces, "setup", "downloads"), 5*time.Minute)
		},
		"control_plane": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			executable := filepath.Join(a.Layout.Bin, workflowhome.ExecutableName)
			start := a.StartControlPlane
			if start == nil {
				start = controlplane.Start
			}
			_, err := start(ctx, controlplane.StartOptions{Layout: a.Layout, Executable: executable, PlatformVersion: effect.Parameters["version"], ApprovedPlanDigestSHA256: a.PlanDigest, Timeout: 30 * time.Second, Replace: effect.Action == "replace"})
			return err
		},
		"create_repository": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			if a.GitHub == nil {
				return errors.New("GitHub client is required")
			}
			_, err := a.GitHub.CreateRepository(ctx, effect.Parameters["owner"], effect.Parameters["authenticated_login"], effect.Parameters["name"], effect.Parameters["private"] == "true")
			if err == nil && a.CreatedRepositories != nil {
				a.CreatedRepositories[effect.Subject] = true
			}
			return err
		},
		"initial_baseline": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			var files []onboarding.BaselineFile
			if err := json.Unmarshal([]byte(effect.Parameters["files_json"]), &files); err != nil {
				return err
			}
			branch := effect.Parameters["branch"]
			if _, err := gitCommandOutput(ctx, effect.Subject, "rev-parse", "--verify", "HEAD"); err != nil {
				message := "Initial Repository Baseline\n\nSetup-Plan-SHA256: " + a.PlanDigest
				if _, err := onboarding.CreateInitialBaseline(ctx, effect.Subject, branch, files, message); err != nil {
					return err
				}
			}
			sourceURL := effect.Parameters["source_url"]
			if sourceURL == "" || effect.Parameters["repository"] == "" {
				return errors.New("Initial Repository Baseline requires the approved repository identity and source URL")
			}
			return onboarding.PublishDefaultBranch(ctx, effect.Subject, sourceURL, branch, a.GitCredential)
		},
		"publish_history": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			return onboarding.PublishDefaultBranch(ctx, a.RepositoryPath, "https://github.com/"+effect.Subject+".git", effect.Parameters["branch"], a.GitCredential)
		},
		"github_label": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			repository := strings.SplitN(effect.Subject, "#", 2)[0]
			label := github.ManagedLabel{Name: effect.Parameters["name"], Color: effect.Parameters["color"], Description: effect.Parameters["description"]}
			existing, err := a.GitHub.Label(ctx, repository, label.Name)
			if github.IsNotFound(err) {
				return a.GitHub.CreateLabel(ctx, repository, label)
			}
			if err != nil {
				return err
			}
			return a.GitHub.UpdateLabel(ctx, repository, existing.Name, label)
		},
		"repository_features": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			if a.GitHub == nil {
				return errors.New("GitHub client is required")
			}
			return a.GitHub.UpdateRepositoryFeatures(ctx, effect.Subject, effect.Parameters["issues"] == "true", effect.Parameters["actions"] == "true", effect.Parameters["allowed_actions"])
		},
		"repository_contract_pr": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			return a.applyRepositoryContract(ctx, effect)
		},
		"repository_admission": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			// Admission is verification plus a durable Engine record; it has no
			// independent remote mutation.
			return nil
		},
		"local_fast_forward": func(a HostAdapter, ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
			if a.GitHub == nil {
				return errors.New("GitHub client is required")
			}
			mergeHead := a.OnboardingMergeHeads[effect.Parameters["merge_head_effect_id"]]
			if !fullSetupCommitID(mergeHead) {
				return errors.New("persisted Onboarding Pull Request merge HEAD is required for local synchronization")
			}
			remote, err := a.GitHub.DefaultBranchHead(ctx, effect.Parameters["repository"])
			if err != nil {
				return err
			}
			if remote.Name != effect.Parameters["branch"] || remote.Head != mergeHead {
				return errors.New("GitHub default branch advanced after the approved onboarding merge")
			}
			expected := effect.Parameters["pre_merge_head"]
			if expected == "" {
				var err error
				expected, err = gitCommandOutput(ctx, effect.Subject, "rev-parse", "--verify", "HEAD")
				if err != nil {
					return err
				}
				message, messageErr := gitCommandOutput(ctx, effect.Subject, "log", "-1", "--format=%B")
				if messageErr != nil || !strings.Contains(message, "Setup-Plan-SHA256: "+a.PlanDigest) {
					return errors.New("local pre-merge baseline is not bound to the approved plan")
				}
			}
			return onboarding.SafeFastForward(ctx, effect.Subject, effect.Parameters["repository"], effect.Parameters["branch"], expected, mergeHead, a.GitCredential)
		},
		"platform_installation": func(HostAdapter, context.Context, setupcontract.Effect, *SecretInput) error {
			return errors.New("Platform Installation effects require Engine durable state")
		},
	}
}

func (a HostAdapter) CheckPrecondition(ctx context.Context, value setupcontract.Precondition) error {
	if err := setupcontract.ValidatePreconditionForExecution(value); err != nil {
		return err
	}
	if value.Kind == "host_identity" {
		expected, err := a.checkHostIdentityBeforeLayout(value)
		if err != nil {
			return err
		}
		ownerID, err := workflowHomeOwnerIdentity(a.Layout.Root)
		if err != nil {
			return fmt.Errorf("read Workflow Home owner identity: %w", err)
		}
		if !strings.EqualFold(expected.WorkflowHomeOwnerID, ownerID) {
			return errors.New("Workflow Home owner identity drifted from the approved plan")
		}
		return nil
	}
	if value.Kind == "github_default_head" {
		if a.GitHub == nil {
			return errors.New("GitHub client is required to verify the approved default-branch base")
		}
		var expected struct {
			Branch         string `json:"branch"`
			Head           string `json:"head"`
			ManifestDigest string `json:"manifest_digest"`
		}
		if err := json.Unmarshal([]byte(value.Expected), &expected); err != nil || expected.Branch == "" || expected.Head == "" {
			return errors.New("approved GitHub default-head precondition is invalid")
		}
		actual, err := a.GitHub.DefaultBranchHead(ctx, value.Subject)
		if err != nil {
			return err
		}
		if actual.Name == expected.Branch && actual.Head == expected.Head {
			return nil
		}
		// A retry after this plan's pull request merged is allowed to resume;
		// the effect readbacks still verify every managed resource exactly.
		if actual.Name == expected.Branch && expected.ManifestDigest != "" {
			manifest, readErr := a.GitHub.RepositoryFile(ctx, value.Subject, repositorycontract.ManifestPath, expected.Branch)
			sum := sha256.Sum256(manifest)
			if readErr == nil && hex.EncodeToString(sum[:]) == expected.ManifestDigest {
				return nil
			}
		}
		return errors.New("GitHub default-branch HEAD drifted from the approved onboarding base")
	}
	if value.Kind == "onboarding_snapshot" {
		transitions := onboarding.ApprovalTransitions{}
		for repository, created := range a.CreatedRepositories {
			if created {
				transitions.CreatedRepository = repository
			}
		}
		for _, head := range a.PublishedHistoryHeads {
			transitions.PublishedHistoryHead = head
		}
		for _, head := range a.InitialBaselineHeads {
			transitions.InitialBaselineHead = head
		}
		for _, head := range a.OnboardingMergeHeads {
			transitions.MergedHead = head
		}
		return onboarding.VerifyApprovalSnapshotTransitions(ctx, value.Expected, transitions)
	}
	if value.Kind == "github_policy" {
		if a.GitHub == nil {
			return errors.New("GitHub client is required to verify repository policy")
		}
		var expected onboarding.RepositoryPolicy
		if json.Unmarshal([]byte(value.Expected), &expected) != nil {
			return errors.New("approved repository policy precondition is invalid")
		}
		branch := ""
		if a.RepositoryPath != "" {
			branch, _ = onboardingGitBranch(ctx, a.RepositoryPath)
		}
		actual, err := a.GitHub.DiscoverPolicy(ctx, value.Subject, branch)
		if err != nil {
			return err
		}
		if expected.AllowFeatureEnable {
			actual.HasIssues, actual.ActionsEnabled = expected.HasIssues, expected.ActionsEnabled
		}
		actual.AllowFeatureEnable = expected.AllowFeatureEnable
		expectedJSON, _ := json.Marshal(expected)
		actualJSON, _ := json.Marshal(actual)
		if string(expectedJSON) != string(actualJSON) {
			return errors.New("GitHub repository policy drifted after planning")
		}
		return nil
	}
	if value.Kind != "git_head" {
		return nil
	}
	command := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD")
	command.Dir = value.Subject
	output, err := command.Output()
	if value.Expected == "" {
		if err == nil {
			actual := strings.TrimSpace(string(output))
			for _, head := range a.InitialBaselineHeads {
				if actual == head {
					return nil
				}
			}
			for _, head := range a.OnboardingMergeHeads {
				if actual == head {
					return nil
				}
			}
			return errors.New("repository gained a commit after planning")
		}
		return nil
	}
	if err != nil || strings.TrimSpace(string(output)) != value.Expected {
		actual := strings.TrimSpace(string(output))
		for _, head := range a.InitialBaselineHeads {
			if actual == head {
				return nil
			}
		}
		for _, head := range a.OnboardingMergeHeads {
			if actual == head {
				return nil
			}
		}
		return errors.New("local Git HEAD drifted after planning")
	}
	return nil
}

// CheckPreLayoutPrecondition fences a Platform Plan to the approved current
// user without reading or creating Workflow Home. CheckPrecondition repeats
// this identity check after layout creation and additionally verifies its real
// filesystem owner.
func (a HostAdapter) CheckPreLayoutPrecondition(_ context.Context, value setupcontract.Precondition) error {
	if err := setupcontract.ValidatePreconditionForExecution(value); err != nil {
		return err
	}
	if value.Kind != "host_identity" {
		return fmt.Errorf("pre-layout verification is unsupported for precondition kind %q", value.Kind)
	}
	_, err := a.checkHostIdentityBeforeLayout(value)
	return err
}

func (a HostAdapter) checkHostIdentityBeforeLayout(value setupcontract.Precondition) (hostIdentityPrecondition, error) {
	if value.Subject != "current-user" {
		return hostIdentityPrecondition{}, errors.New("host identity precondition subject must be current-user")
	}
	var expected hostIdentityPrecondition
	if err := json.Unmarshal([]byte(value.Expected), &expected); err != nil || expected.UserID == "" || expected.Username == "" || expected.WorkflowHome == "" || expected.WorkflowHomeOwnerID == "" {
		return hostIdentityPrecondition{}, errors.New("approved host identity precondition is invalid")
	}
	if a.Layout.Root == "" || !strings.EqualFold(filepath.Clean(expected.WorkflowHome), filepath.Clean(a.Layout.Root)) {
		return hostIdentityPrecondition{}, errors.New("approved host identity is not bound to the target Workflow Home")
	}
	current, err := user.Current()
	if err != nil {
		return hostIdentityPrecondition{}, fmt.Errorf("read current host identity: %w", err)
	}
	if expected.UserID != current.Uid || !strings.EqualFold(expected.Username, current.Username) {
		return hostIdentityPrecondition{}, errors.New("current host identity drifted from the approved plan")
	}
	return expected, nil
}

func gitCommandOutput(ctx context.Context, repository string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repository
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func onboardingGitBranch(ctx context.Context, repository string) (string, error) {
	command := exec.CommandContext(ctx, "git", "branch", "--show-current")
	command.Dir = repository
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func (a HostAdapter) applyRepositoryContract(ctx context.Context, effect setupcontract.Effect) (resultErr error) {
	if a.GitHub == nil || a.PlanDigest == "" {
		return errors.New("GitHub client and approved plan digest are required")
	}
	if a.OnboardingMergeHeads == nil {
		return errors.New("durable Onboarding Pull Request merge-HEAD binding is required")
	}
	var encoded map[string]string
	if err := json.Unmarshal([]byte(effect.Parameters["files_json"]), &encoded); err != nil {
		return err
	}
	files := map[string][]byte{}
	for path, value := range encoded {
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return err
		}
		files[path] = data
	}
	baseHead := effect.Parameters["base_head"]
	if baseHead == "" {
		revision, err := a.GitHub.DefaultBranchHead(ctx, effect.Subject)
		if err != nil {
			return err
		}
		baseHead = revision.Head
	}
	if err := a.requireOnboardingBase(ctx, effect.Subject, effect.Parameters["base_branch"], baseHead); err != nil {
		return err
	}
	temporary := a.TemporaryRoot
	if temporary == "" {
		temporary = a.Layout.Workspaces
	}
	workspace, err := onboarding.PrepareOnboardingBranch(ctx, effect.Subject, effect.Parameters["source_url"], baseHead, temporary, a.PlanDigest, files, a.GitCredential)
	if err != nil {
		return err
	}
	cleanupWorkspace := a.CleanupOnboardingWorkspace
	if cleanupWorkspace == nil {
		cleanupWorkspace = func(workspace onboarding.GitWorkspace) error { return workspace.Cleanup() }
	}
	defer func() {
		branchErr := a.GitHub.DeleteBranch(context.Background(), effect.Subject, workspace.Branch)
		if github.IsNotFound(branchErr) {
			branchErr = nil
		}
		if branchErr != nil {
			branchErr = fmt.Errorf("cleanup remote onboarding branch: %w", branchErr)
		}
		workspaceErr := cleanupWorkspace(workspace)
		if workspaceErr != nil {
			workspaceErr = fmt.Errorf("cleanup temporary onboarding clone: %w", workspaceErr)
		}
		resultErr = errors.Join(resultErr, branchErr, workspaceErr)
	}()
	body := "Approved Setup Plan SHA-256: " + a.PlanDigest
	owner := strings.SplitN(effect.Subject, "/", 2)[0]
	pull, found, err := a.GitHub.FindOnboardingPullRequest(ctx, effect.Subject, owner, workspace.Branch, effect.Parameters["base_branch"])
	if err != nil {
		return err
	}
	if found {
		if pull.Head.SHA != workspace.Head || pull.Body != body || pull.Base.Ref != "" && (pull.Base.Ref != effect.Parameters["base_branch"] || pull.Base.SHA != baseHead) {
			return errors.New("existing Onboarding Pull Request differs from the approved plan")
		}
	} else {
		if err := a.requireOnboardingBase(ctx, effect.Subject, effect.Parameters["base_branch"], baseHead); err != nil {
			return err
		}
		pull, err = a.GitHub.CreateOnboardingPullRequest(ctx, effect.Subject, github.PullRequestCreate{Title: "Onboard Agent Workflow", Head: workspace.Branch, Base: effect.Parameters["base_branch"], Body: body})
		if err != nil {
			return err
		}
	}
	var requiredChecks []onboarding.RequiredCheck
	if err := json.Unmarshal([]byte(effect.Parameters["required_checks_json"]), &requiredChecks); err != nil || len(requiredChecks) == 0 {
		return errors.New("Onboarding Pull Request lacks approved required checks")
	}
	checkDiagnostics, err := waitForOnboardingChecks(ctx, a.GitHub, effect.Subject, workspace.Head, requiredChecks, 10*time.Minute)
	if a.OnboardingCheckDiagnostics != nil {
		a.OnboardingCheckDiagnostics[effect.ID] = checkDiagnostics
	}
	if err != nil {
		return err
	}
	current, err := a.GitHub.OnboardingPullRequest(ctx, effect.Subject, pull.Number)
	if err != nil {
		return err
	}
	if current.Head.SHA != workspace.Head || current.Base.Ref != effect.Parameters["base_branch"] || current.Base.SHA != baseHead || current.Mergeable == nil || !*current.Mergeable {
		return errors.New("Onboarding Pull Request drifted or is not mergeable")
	}
	reviews, err := a.GitHub.OnboardingPullRequestReviews(ctx, effect.Subject, pull.Number)
	if err != nil {
		return err
	}
	for _, review := range reviews {
		if strings.EqualFold(review.State, "CHANGES_REQUESTED") {
			return errors.New("Onboarding Pull Request has requested changes")
		}
	}
	repository, err := a.GitHub.RepositoryForOnboarding(ctx, effect.Subject)
	if err != nil {
		return err
	}
	method := ""
	switch {
	case repository.AllowSquashMerge:
		method = "squash"
	case repository.AllowMergeCommit:
		method = "merge"
	case repository.AllowRebaseMerge:
		method = "rebase"
	default:
		return errors.New("repository has no supported merge method")
	}
	// Re-read both sides immediately before exercising the approved merge
	// authority. Checks and repository-policy reads above may take minutes.
	current, err = a.GitHub.OnboardingPullRequest(ctx, effect.Subject, pull.Number)
	if err != nil {
		return err
	}
	if current.Head.SHA != workspace.Head || current.Base.Ref != effect.Parameters["base_branch"] || current.Base.SHA != baseHead || current.Mergeable == nil || !*current.Mergeable {
		return errors.New("Onboarding Pull Request drifted before merge")
	}
	if err := a.requireOnboardingBase(ctx, effect.Subject, effect.Parameters["base_branch"], baseHead); err != nil {
		return err
	}
	merge, err := a.GitHub.MergeOnboardingPullRequest(ctx, effect.Subject, pull.Number, workspace.Head, method)
	if err != nil {
		return err
	}
	if !fullSetupCommitID(merge.SHA) {
		return errors.New("GitHub merge response lacks the actual default-branch HEAD")
	}
	a.OnboardingMergeHeads[effect.ID] = merge.SHA
	mergedDefault, err := a.GitHub.DefaultBranchHead(ctx, effect.Subject)
	if err != nil {
		return err
	}
	if mergedDefault.Name != effect.Parameters["base_branch"] || mergedDefault.Head != merge.SHA {
		return fmt.Errorf("%s%s; GitHub default branch advanced before the approved merge HEAD could be bound", onboardingMergeHeadEvidence, merge.SHA)
	}
	return nil
}

func fullSetupCommitID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (a HostAdapter) requireOnboardingBase(ctx context.Context, repository, branch, head string) error {
	if a.GitHub == nil || branch == "" || head == "" {
		return errors.New("approved Onboarding Pull Request base is incomplete")
	}
	actual, err := a.GitHub.DefaultBranchHead(ctx, repository)
	if err != nil {
		return err
	}
	if actual.Name != branch || actual.Head != head {
		return errors.New("GitHub default-branch base drifted from the approved Onboarding Plan")
	}
	return nil
}

func waitForOnboardingChecks(ctx context.Context, client *github.Client, repository, head string, required []onboarding.RequiredCheck, timeout time.Duration) ([]string, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		checks, err := client.OnboardingChecks(deadline, repository, head)
		if err != nil {
			return nil, err
		}
		requiredSet := make(map[string]onboarding.RequiredCheck, len(required))
		for _, binding := range required {
			if binding.Context == "" || binding.AppID <= 0 {
				return nil, errors.New("approved required check lacks an App identity")
			}
			if existing, ok := requiredSet[binding.Context]; ok && existing.AppID != binding.AppID {
				return nil, errors.New("approved required check context has conflicting App identities")
			}
			requiredSet[binding.Context] = binding
		}
		passed := map[string]bool{}
		diagnostics := make([]string, 0, len(checks))
		requiredObserved := map[string]string{}
		for _, check := range checks {
			binding, isRequired := requiredSet[check.Name]
			if !isRequired {
				diagnostics = append(diagnostics, fmt.Sprintf("optional check %q: status=%s conclusion=%s", check.Name, check.Status, check.Conclusion))
				continue
			}
			if check.AppID != binding.AppID {
				diagnostics = append(diagnostics, fmt.Sprintf("same-name check %q from unapproved App %d (want %d)", check.Name, check.AppID, binding.AppID))
				continue
			}
			if check.HeadSHA != head {
				diagnostics = append(diagnostics, fmt.Sprintf("required check %q reports unapproved head %q", check.Name, check.HeadSHA))
				continue
			}
			requiredObserved[check.Name] = "status=" + check.Status + " conclusion=" + check.Conclusion
			if check.Status != "completed" {
				continue
			}
			switch check.Conclusion {
			case "success", "neutral", "skipped":
			default:
				return diagnostics, fmt.Errorf("required pull request check %q concluded %q", check.Name, check.Conclusion)
			}
			if check.Conclusion == "success" {
				passed[check.Name] = true
			}
		}
		allRequired := true
		for _, binding := range required {
			if !passed[binding.Context] {
				allRequired = false
				break
			}
		}
		if allRequired {
			return diagnostics, nil
		}
		select {
		case <-deadline.Done():
			states := make([]string, 0, len(required))
			for _, binding := range required {
				name := binding.Context
				state := requiredObserved[name]
				if state == "" {
					state = "not observed"
				}
				states = append(states, name+" ("+state+")")
			}
			return diagnostics, fmt.Errorf("wait for onboarding checks: %w; required check states: %s", deadline.Err(), strings.Join(states, ", "))
		case <-ticker.C:
		}
	}
}

func skillBundleSpec(effect setupcontract.Effect) (workflowhome.SkillBundleSpec, error) {
	var skills []string
	if err := json.Unmarshal([]byte(effect.Parameters["managed_skills_json"]), &skills); err != nil {
		return workflowhome.SkillBundleSpec{}, fmt.Errorf("decode managed Workflow skills: %w", err)
	}
	var files []workflowhome.SkillBundleFile
	if err := json.Unmarshal([]byte(effect.Parameters["files_json"]), &files); err != nil {
		return workflowhome.SkillBundleSpec{}, fmt.Errorf("decode Workflow Skill Bundle files: %w", err)
	}
	return workflowhome.SkillBundleSpec{Version: effect.Parameters["version"], DestinationRoot: effect.Subject, ManagedSkills: skills, Files: files}, nil
}
