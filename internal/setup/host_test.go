package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

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
	effect := setupcontract.Effect{ID: "install", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0"}}
	if err := adapter.Apply(context.Background(), effect, &SecretInput{}); err != nil {
		t.Fatal(err)
	}
	if persisted != layout.Bin {
		t.Fatalf("persisted PATH entry = %q, want %q", persisted, layout.Bin)
	}
}

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
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "pat-plan", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "release", Subject: "v1", Expected: "ok"}}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: layout.CredentialFile, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "owner", "api_base": server.URL}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform", Subject: layout.Root, Expected: "ready"}}}
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
