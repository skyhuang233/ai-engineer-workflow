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
	Layout              workflowhome.Layout
	Executable          string
	SkillBundleSource   string
	PersistUserPATH     func(string) error
	GitHub              *github.Client
	GitCredential       onboarding.GitCredential
	RepositoryPath      string
	PlanDigest          string
	TemporaryRoot       string
	StartControlPlane   func(context.Context, controlplane.StartOptions) (controlplane.RuntimeRecord, error)
	InspectControlPlane func(context.Context, *controlplane.RuntimeRecord) controlplane.Observation
}

func (a HostAdapter) Readback(ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	switch effect.Kind {
	case "github_pat":
		_, err := credential.NewFileStore(a.Layout.CredentialFile).Get(ctx, credential.GatewayTarget)
		if errors.Is(err, credential.ErrNotFound) {
			return setupcontract.EffectRequired, "credential file is absent", nil
		}
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		return setupcontract.EffectSatisfied, "credential file exists", nil
	case "platform_cli":
		path := filepath.Join(a.Layout.Bin, workflowhome.ExecutableName)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return setupcontract.EffectRequired, "platform CLI is absent", nil
		} else if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		reconciled, err := workflowhome.CurrentUserPathIsReconciled(a.Layout.Bin)
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if !reconciled {
			return setupcontract.EffectRequired, "platform CLI exists but current-user PATH needs reconciliation", nil
		}
		return setupcontract.EffectSatisfied, "platform CLI exists", nil
	case "workflow_skill_bundle":
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
	case "docker_desktop":
		command := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
		output, err := command.CombinedOutput()
		if err != nil {
			return setupcontract.EffectRequired, "Docker Desktop engine is unavailable", nil
		}
		if strings.TrimSpace(string(output)) != "linux/amd64" && strings.TrimSpace(string(output)) != "linux/x86_64" {
			return setupcontract.EffectConflicting, "Docker engine is not Linux amd64", nil
		}
		return setupcontract.EffectSatisfied, "Docker Linux amd64 engine is ready", nil
	case "control_plane":
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
			return setupcontract.EffectConflicting, "a different approved Control Plane instance is running", nil
		}
		return setupcontract.EffectSatisfied, "Control Plane process identity and health are verified", nil
	case "create_repository":
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
		return setupcontract.EffectSatisfied, "GitHub repository exists with approved identity and visibility", nil
	case "initial_baseline":
		head, err := gitCommandOutput(ctx, effect.Subject, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return setupcontract.EffectRequired, "Initial Repository Baseline is absent", nil
		}
		message, err := gitCommandOutput(ctx, effect.Subject, "log", "-1", "--format=%B")
		if err != nil || !strings.Contains(message, "Setup-Plan-SHA256: "+a.PlanDigest) {
			return setupcontract.EffectConflicting, "existing baseline is not bound to the approved plan", nil
		}
		var approved []string
		if json.Unmarshal([]byte(effect.Parameters["files_json"]), &approved) != nil {
			return setupcontract.EffectFailed, "", errors.New("Initial Repository Baseline file contract is invalid")
		}
		tree, err := gitCommandOutput(ctx, effect.Subject, "ls-tree", "-r", "--name-only", head)
		if err != nil || strings.Join(approved, "\n") != strings.TrimSpace(tree) {
			return setupcontract.EffectConflicting, "Initial Repository Baseline tree differs", nil
		}
		if a.GitHub == nil {
			return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
		}
		remote, err := a.GitHub.DefaultBranchHead(ctx, effect.Parameters["repository"])
		if github.IsNotFound(err) || err == nil && remote.Head != head {
			return setupcontract.EffectRequired, "Initial Repository Baseline is not published", nil
		}
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		return setupcontract.EffectSatisfied, "approved Initial Repository Baseline is published", nil
	case "publish_history":
		if a.GitHub == nil {
			return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
		}
		revision, err := a.GitHub.DefaultBranchHead(ctx, effect.Subject)
		if github.IsNotFound(err) {
			return setupcontract.EffectRequired, "committed history is not published", nil
		}
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if revision.Head != effect.Parameters["head"] {
			return setupcontract.EffectConflicting, "published default branch differs from approved committed history", nil
		}
		return setupcontract.EffectSatisfied, "approved committed history is published", nil
	case "github_label":
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
	case "repository_features":
		if a.GitHub == nil {
			return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
		}
		policy, err := a.GitHub.DiscoverPolicy(ctx, effect.Subject, "")
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		if !policy.HasIssues || !policy.ActionsEnabled {
			return setupcontract.EffectRequired, "GitHub Issues or Actions is disabled", nil
		}
		return setupcontract.EffectSatisfied, "GitHub Issues and Actions are enabled", nil
	case "repository_contract_pr":
		if a.GitHub == nil {
			return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
		}
		content, err := a.GitHub.RepositoryFile(ctx, effect.Subject, repositorycontract.ManifestPath, effect.Parameters["base_branch"])
		if github.IsNotFound(err) {
			return setupcontract.EffectRequired, "Repository Contract Manifest is absent", nil
		}
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != effect.Parameters["manifest_digest"] {
			return setupcontract.EffectRequired, "Repository Contract Manifest differs", nil
		}
		return setupcontract.EffectSatisfied, "Repository Contract Manifest is merged", nil
	case "repository_admission":
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
		if err != nil || !policy.HasIssues || !policy.ActionsEnabled {
			return setupcontract.EffectRequired, "GitHub Issues or Actions is not available", nil
		}
		return setupcontract.EffectSatisfied, "Repository Contract and managed GitHub resources are verified", nil
	case "local_fast_forward":
		if a.GitHub == nil {
			return setupcontract.EffectFailed, "", errors.New("GitHub client is required")
		}
		remote, err := a.GitHub.DefaultBranchHead(ctx, effect.Parameters["repository"])
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		local, err := gitCommandOutput(ctx, effect.Subject, "rev-parse", "--verify", effect.Parameters["branch"])
		if err == nil && local == remote.Head {
			return setupcontract.EffectSatisfied, "local default branch matches admitted GitHub branch", nil
		}
		return setupcontract.EffectRequired, "local default branch requires a safe fast-forward", nil
	default:
		return setupcontract.EffectConflicting, "unknown effect kind", nil
	}
}

func (a HostAdapter) Apply(ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
	switch effect.Kind {
	case "github_pat":
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
	case "platform_cli":
		source := a.Executable
		if source == "" {
			source, _ = os.Executable()
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		version := effect.Parameters["version"]
		if version == "" {
			return errors.New("platform CLI effect requires a version")
		}
		if err := (workflowhome.Installation{Layout: a.Layout}).InstallVersion(version, source, hex.EncodeToString(sum[:])); err != nil {
			return err
		}
		persist := a.PersistUserPATH
		if persist == nil {
			persist = workflowhome.PersistCurrentUserPath
		}
		return persist(a.Layout.Bin)
	case "workflow_skill_bundle":
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
	case "docker_desktop":
		contract := hostsetup.DockerDesktopContract{Version: effect.Parameters["version"], InstallerURL: effect.Parameters["installer_url"], WindowsAMD64SHA256: effect.Parameters["windows_amd64_sha256"]}
		return hostsetup.EnsureDockerDesktop(ctx, contract, hostsetup.WindowsDockerDesktopHost{}, filepath.Join(a.Layout.Workspaces, "setup", "downloads"), 5*time.Minute)
	case "control_plane":
		executable := filepath.Join(a.Layout.Bin, workflowhome.ExecutableName)
		start := a.StartControlPlane
		if start == nil {
			start = controlplane.Start
		}
		_, err := start(ctx, controlplane.StartOptions{Layout: a.Layout, Executable: executable, PlatformVersion: effect.Parameters["version"], ApprovedPlanDigestSHA256: a.PlanDigest, Timeout: 30 * time.Second})
		return err
	case "create_repository":
		if a.GitHub == nil {
			return errors.New("GitHub client is required")
		}
		_, err := a.GitHub.CreateRepository(ctx, effect.Parameters["owner"], effect.Parameters["authenticated_login"], effect.Parameters["name"], effect.Parameters["private"] == "true")
		return err
	case "initial_baseline":
		var files []string
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
	case "publish_history":
		return onboarding.PublishDefaultBranch(ctx, a.RepositoryPath, "https://github.com/"+effect.Subject+".git", effect.Parameters["branch"], a.GitCredential)
	case "github_label":
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
	case "repository_features":
		if a.GitHub == nil {
			return errors.New("GitHub client is required")
		}
		return a.GitHub.UpdateRepositoryFeatures(ctx, effect.Subject, effect.Parameters["issues"] == "true", effect.Parameters["actions"] == "true")
	case "repository_contract_pr":
		return a.applyRepositoryContract(ctx, effect)
	case "repository_admission":
		// Admission is verification plus a durable Engine record; it has no
		// independent remote mutation.
		return nil
	case "local_fast_forward":
		return onboarding.SafeFastForward(ctx, effect.Subject, effect.Parameters["branch"], a.GitCredential)
	default:
		return fmt.Errorf("unsupported Setup effect kind %q", effect.Kind)
	}
}

func (a HostAdapter) CheckPrecondition(ctx context.Context, value setupcontract.Precondition) error {
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
			message, messageErr := gitCommandOutput(ctx, value.Subject, "log", "-1", "--format=%B")
			if messageErr == nil && strings.Contains(message, "Setup-Plan-SHA256: "+a.PlanDigest) {
				return nil
			}
			return errors.New("repository gained a commit after planning")
		}
		return nil
	}
	if err != nil || strings.TrimSpace(string(output)) != value.Expected {
		return errors.New("local Git HEAD drifted after planning")
	}
	return nil
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

func (a HostAdapter) applyRepositoryContract(ctx context.Context, effect setupcontract.Effect) error {
	if a.GitHub == nil || a.PlanDigest == "" {
		return errors.New("GitHub client and approved plan digest are required")
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
	temporary := a.TemporaryRoot
	if temporary == "" {
		temporary = a.Layout.Workspaces
	}
	workspace, err := onboarding.PrepareOnboardingBranch(ctx, effect.Parameters["source_url"], baseHead, temporary, a.PlanDigest, files, a.GitCredential)
	if err != nil {
		return err
	}
	defer workspace.Cleanup()
	body := "Approved Setup Plan SHA-256: " + a.PlanDigest
	owner := strings.SplitN(effect.Subject, "/", 2)[0]
	pull, found, err := a.GitHub.FindOnboardingPullRequest(ctx, effect.Subject, owner, workspace.Branch, effect.Parameters["base_branch"])
	if err != nil {
		return err
	}
	if found {
		if pull.Head.SHA != workspace.Head || pull.Body != body {
			return errors.New("existing Onboarding Pull Request differs from the approved plan")
		}
	} else {
		pull, err = a.GitHub.CreateOnboardingPullRequest(ctx, effect.Subject, github.PullRequestCreate{Title: "Onboard Agent Workflow", Head: workspace.Branch, Base: effect.Parameters["base_branch"], Body: body})
		if err != nil {
			return err
		}
	}
	defer a.GitHub.DeleteBranch(context.Background(), effect.Subject, workspace.Branch)
	var requiredChecks []string
	if err := json.Unmarshal([]byte(effect.Parameters["required_checks_json"]), &requiredChecks); err != nil || len(requiredChecks) == 0 {
		return errors.New("Onboarding Pull Request lacks approved required checks")
	}
	if err := waitForOnboardingChecks(ctx, a.GitHub, effect.Subject, workspace.Head, requiredChecks, 10*time.Minute); err != nil {
		return err
	}
	current, err := a.GitHub.OnboardingPullRequest(ctx, effect.Subject, pull.Number)
	if err != nil {
		return err
	}
	if current.Head.SHA != workspace.Head || current.Mergeable == nil || !*current.Mergeable {
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
	if _, err := a.GitHub.MergeOnboardingPullRequest(ctx, effect.Subject, pull.Number, workspace.Head, method); err != nil {
		return err
	}
	return nil
}

func waitForOnboardingChecks(ctx context.Context, client *github.Client, repository, head string, required []string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		checks, err := client.PullRequestChecks(deadline, repository, head)
		if err != nil {
			return err
		}
		passed := map[string]bool{}
		pending := false
		for _, check := range checks {
			if check.Status != "completed" {
				pending = true
				continue
			}
			switch check.Conclusion {
			case "success", "neutral", "skipped":
			default:
				return fmt.Errorf("pull request check %q concluded %q", check.Name, check.Conclusion)
			}
			if check.Conclusion == "success" {
				passed[check.Name] = true
			}
		}
		allRequired := true
		for _, name := range required {
			if !passed[name] {
				allRequired = false
				break
			}
		}
		if allRequired && !pending {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("wait for onboarding checks: %w", deadline.Err())
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
