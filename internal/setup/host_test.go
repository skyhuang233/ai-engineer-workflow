package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

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
