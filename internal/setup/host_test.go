package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/credential"
	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestRepositoryContractRechecksDefaultHeadImmediatelyBeforePRCreation(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	hostGit(t, "", "init", "-b", "main", source)
	hostGit(t, source, "config", "user.name", "Test")
	hostGit(t, source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostGit(t, source, "add", "README.md")
	hostGit(t, source, "commit", "-m", "base")
	base := hostGitOutput(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	hostGit(t, "", "clone", "--bare", source, bare)

	refReads := 0
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			refReads++
			head := base
			if refReads > 1 {
				head = strings.Repeat("b", 40)
			}
			_, _ = w.Write([]byte(`{"object":{"sha":"` + head + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			created = true
			_, _ = w.Write([]byte(`{"number":7}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/git/refs/heads/workflow/onboarding-"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	files, _ := json.Marshal(map[string]string{"managed.txt": base64.StdEncoding.EncodeToString([]byte("contract\n"))})
	effect := setupcontract.Effect{Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{
		"files_json": string(files), "source_url": bare, "base_head": base, "base_branch": "main", "required_checks_json": `["workflow-contract"]`,
	}}
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner"), PlanDigest: strings.Repeat("a", 64), TemporaryRoot: t.TempDir(), OnboardingMergeHeads: map[string]string{}}
	err := adapter.applyRepositoryContract(context.Background(), effect)
	if err == nil || !strings.Contains(err.Error(), "base drifted") {
		t.Fatalf("default-head drift accepted: %v", err)
	}
	if created {
		t.Fatal("pull request was created after the approved base drifted")
	}
}

func TestRepositoryContractReadbackBindsMergedDefaultHeadAndRejectsLaterCommit(t *testing.T) {
	digest := strings.Repeat("d", 64)
	mergedHead := strings.Repeat("a", 40)
	defaultHead := mergedHead
	manifest := []byte(`{"schema_version":1}`)
	manifestSum := sha256.Sum256(manifest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/.workflow/repository.json":
			_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString(manifest) + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			if r.URL.Query().Get("state") != "all" {
				t.Fatalf("pull request readback state = %q", r.URL.Query().Get("state"))
			}
			_, _ = w.Write([]byte(`[{"number":7,"body":"Approved Setup Plan SHA-256: ` + digest + `","merged_at":"2026-08-15T00:00:00Z","merge_commit_sha":"` + mergedHead + `","head":{"sha":"` + strings.Repeat("b", 40) + `","ref":"workflow/onboarding-` + digest[:12] + `"},"base":{"sha":"` + strings.Repeat("c", 40) + `","ref":"main"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"` + defaultHead + `"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	adapter := HostAdapter{GitHub: workflowgithub.NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner"), PlanDigest: digest, OnboardingMergeHeads: map[string]string{}}
	effect := setupcontract.Effect{ID: "contract", Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{"base_branch": "main", "manifest_digest": hex.EncodeToString(manifestSum[:])}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("merged readback = %s, %v", status, err)
	}
	if adapter.OnboardingMergeHeads[effect.ID] != mergedHead {
		t.Fatalf("durable merge binding = %#v", adapter.OnboardingMergeHeads)
	}
	restored := HostAdapter{OnboardingMergeHeads: map[string]string{}}
	if err := restored.RestoreEffectResults([]setupcontract.EffectResult{{EffectID: effect.ID, Evidence: onboardingMergeHeadEvidence + mergedHead}}); err != nil || restored.OnboardingMergeHeads[effect.ID] != mergedHead {
		t.Fatalf("restored merge binding = %#v, %v", restored.OnboardingMergeHeads, err)
	}
	defaultHead = strings.Repeat("e", 40)
	status, evidence, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting || !strings.Contains(evidence, "advanced") {
		t.Fatalf("post-merge extra commit readback = %s, %q, %v", status, evidence, err)
	}
}

func hostGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func hostGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func TestHostAdapterPersistsCurrentUserPathAfterInstallingCLI(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(source, []byte("workflow executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	var persisted string
	adapter := HostAdapter{Layout: layout, Executable: source, PersistUserPATH: func(path string) error {
		persisted = path
		return nil
	}}
	digest := sha256.Sum256([]byte("workflow executable"))
	effect := setupcontract.Effect{ID: "install", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0", "sha256": hex.EncodeToString(digest[:])}}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	if persisted != layout.Bin {
		t.Fatalf("persisted PATH entry = %q, want %q", persisted, layout.Bin)
	}
}

func TestHostAdapterReadbackRequiresExactOwnedCLIVersionAndChecksum(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "workflow.exe")
	contents := []byte("workflow executable")
	if err := os.WriteFile(source, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	adapter := HostAdapter{Layout: layout, Executable: source, PersistUserPATH: func(string) error { return nil }, CurrentUserPATHReconciled: func(string) (bool, error) { return true, nil }}
	effect := setupcontract.Effect{ID: "install", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0", "sha256": hex.EncodeToString(sum[:])}}
	if err := adapter.Apply(context.Background(), effect, nil); err != nil {
		t.Fatal(err)
	}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("exact readback=%s %v", status, err)
	}
	effect.Parameters["version"] = "1.0.1"
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong version readback=%s %v", status, err)
	}
	effect.Parameters["version"] = "1.0.0"
	effect.Parameters["sha256"] = strings.Repeat("0", 64)
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong checksum readback=%s %v", status, err)
	}
}

func TestHostAdapterDockerReadbackRequiresExactDesktopAndLinuxAMD64Engine(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	host := &setupDockerHost{version: "4.44.0", engineErr: errors.New("Docker engine is windows/amd64")}
	adapter := HostAdapter{Layout: layout, DockerDesktopHost: host}
	effect := setupcontract.Effect{ID: "docker", Kind: "docker_desktop", Subject: "current-host", Action: "repair", Parameters: map[string]string{"version": "4.45.0"}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong version readback=%s %v", status, err)
	}
	host.version = "4.45.0"
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("wrong engine readback=%s %v", status, err)
	}
	host.engineErr = nil
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("compatible Docker readback=%s %v", status, err)
	}
}

type setupDockerHost struct {
	version   string
	engineErr error
}

func (h *setupDockerHost) InstalledVersion(context.Context) (string, error) { return h.version, nil }
func (*setupDockerHost) Download(context.Context, string, string) error     { return nil }
func (*setupDockerHost) InstallElevated(context.Context, string) error      { return nil }
func (*setupDockerHost) Start(context.Context) error                        { return nil }
func (h *setupDockerHost) EngineReady(context.Context) error                { return h.engineErr }

func TestHostAdapterStartsAndReadsBackDigestBoundControlPlane(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	record := controlplane.RuntimeRecord{PID: 42, PlatformVersion: "1.0.0", ProcessStartedAt: time.Now().UTC().Round(0), Endpoints: controlplane.Endpoints{Health: "http://127.0.0.1:1234/health", Shutdown: "http://127.0.0.1:1234/shutdown"}, ApprovedPlanDigestSHA256: digest}
	started := false
	adapter := HostAdapter{
		Layout: layout, PlanDigest: digest,
		StartControlPlane: func(_ context.Context, options controlplane.StartOptions) (controlplane.RuntimeRecord, error) {
			started = options.PlatformVersion == "1.0.0" && options.ApprovedPlanDigestSHA256 == digest
			return record, controlplane.WriteRuntimeRecord(layout, record)
		},
		InspectControlPlane: func(context.Context, *controlplane.RuntimeRecord) controlplane.Observation {
			return controlplane.Observation{State: controlplane.StateReady, Record: &record}
		},
	}
	effect := setupcontract.Effect{ID: "serve", Kind: "control_plane", Subject: layout.Root, Action: "start", Parameters: map[string]string{"version": "1.0.0"}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("initial readback = %s, %v", status, err)
	}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied || !started {
		t.Fatalf("final readback = %s, %v, started=%v", status, err, started)
	}
}

func TestHostAdapterInstallsExactWorkflowSkillBundle(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(source, "agent-workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("# Agent Workflow")
	if err := os.WriteFile(filepath.Join(source, "agent-workflow", "SKILL.md"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	files, _ := json.Marshal([]workflowhome.SkillBundleFile{{Path: "agent-workflow/SKILL.md", SHA256: hex.EncodeToString(digest[:])}})
	skills, _ := json.Marshal([]string{"agent-workflow"})
	effect := setupcontract.Effect{
		ID: "skills", Kind: "workflow_skill_bundle", Subject: filepath.Join(t.TempDir(), "codex-skills"), Action: "install",
		Parameters: map[string]string{"version": "1.0.0", "managed_skills_json": string(skills), "files_json": string(files)},
	}
	adapter := HostAdapter{Layout: layout, SkillBundleSource: source}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("initial bundle readback = %s, %v", status, err)
	}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("installed bundle readback = %s, %v", status, err)
	}
}

func TestHostAdapterPersistsPATOnlyThroughSecretInput(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	adapter := HostAdapter{Layout: layout}
	effect := setupcontract.Effect{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin"}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("initial=%s %v", status, err)
	}
	input := &SecretInput{Reader: bytes.NewBufferString("ghp_secret\n")}
	if err := adapter.Apply(context.Background(), effect, input); err != nil {
		t.Fatal(err)
	}
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("readback=%s %v", status, err)
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(context.Background(), credential.GatewayTarget)
	if err != nil || token != "ghp_secret" {
		t.Fatalf("token=%q %v", token, err)
	}
}

func TestEnginePersistsOnlyVerifiedPATMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = w.Write([]byte(`{"login":"owner","id":7}`))
	}))
	defer server.Close()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "pat-plan", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "release", Subject: "v1", Expected: "ok"}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "api_base": server.URL, "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform", Subject: layout.Root, Expected: "ready"}}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: &SecretInput{Reader: bytes.NewBufferString("ghp_secret")}}
	result, err := engine.Apply(context.Background(), raw, digest)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("result=%#v", result)
	}
	db, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	verification, err := db.GitHubPATVerification(context.Background())
	if err != nil || verification.Login != "owner" {
		t.Fatalf("verification=%#v %v", verification, err)
	}
}

func TestEngineReplacesPersistedPATThatFailsLiveVerification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_replacement" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = w.Write([]byte(`{"login":"owner","id":7}`))
	}))
	defer server.Close()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := credential.NewFileStore(layout.CredentialFile).Set(context.Background(), credential.GatewayTarget, "ghp_revoked"); err != nil {
		t.Fatal(err)
	}
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "replace-pat", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "release", Subject: "v1", Expected: "verified"}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "replace", Parameters: map[string]string{"input": "stdin", "owner": "owner", "api_base": server.URL, "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": repeat("b", 64), "workflow_cli_sha256": repeat("c", 64)}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform", Subject: layout.Root, Expected: "ready"}}}
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Engine{Adapter: HostAdapter{Layout: layout}, SecretInput: &SecretInput{Reader: bytes.NewBufferString("ghp_replacement")}}).Apply(context.Background(), raw, digest)
	if err != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("replacement result=%#v err=%v", result, err)
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(context.Background(), credential.GatewayTarget)
	if err != nil || token != "ghp_replacement" {
		t.Fatalf("replacement token=%q err=%v", token, err)
	}
}
