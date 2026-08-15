package main

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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestOnboardingStateRequiresEveryManagedContractSurface(t *testing.T) {
	files, _, digest, err := repositorycontract.Render("single-context", []byte("# User instructions\n"), "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	driftPath := "docs/agents/domain.md"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/repos/owner/repo/contents/"
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		data, ok := files[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		if path == driftPath {
			data = []byte("managed drift\n")
		}
		_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString(data) + `"}`))
	}))
	defer server.Close()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	state, err := (onboardingCurrentState{Client: github.NewClient(server.URL, "token", server.Client()), Store: database}).DiscoverOnboardingState(context.Background(), "owner/repo", "main", digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.ContractSatisfied {
		t.Fatal("manifest-only match hid managed file drift")
	}
}

func TestSetupPlanReportsBootstrapBlockerWithoutMutatingRepository(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	repo := t.TempDir()
	var output bytes.Buffer
	if err := runSetupPlan([]string{"--repo", repo, "--workflow-home", home}, &output); err != nil {
		t.Fatal(err)
	}
	var response setupResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "blocked" || response.Blocker == "" {
		t.Fatalf("response=%#v", response)
	}
}

func TestInspectPlatformLiveValidatesPersistedPATWithoutSecretInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_persisted" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = w.Write([]byte(`{"login":"owner","id":7}`))
	}))
	defer server.Close()
	originalBase, originalClient := setupInspectionAPIBase, setupInspectionHTTPClient
	setupInspectionAPIBase, setupInspectionHTTPClient = server.URL, server.Client()
	t.Cleanup(func() { setupInspectionAPIBase, setupInspectionHTTPClient = originalBase, originalClient })
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token := "ghp_persisted"
	if err := credential.NewFileStore(layout.CredentialFile).Set(ctx, credential.GatewayTarget, token); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	db, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "owner", UserID: 7, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(executable, []byte("workflow-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	cliSum := sha256.Sum256([]byte("workflow-cli"))
	cliDigest := hex.EncodeToString(cliSum[:])
	if err := (workflowhome.Installation{Layout: layout}).InstallVersion("1.0.0", executable, cliDigest); err != nil {
		t.Fatal(err)
	}
	contract := []byte(`{}`)
	_, contractDigest, _ := setupcontract.Canonicalize(contract)
	if err := os.WriteFile(filepath.Join(layout.Config, "platform-setup-contract.json"), contract, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseDigest := strings.Repeat("a", 64)
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "platform-inspection", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "platform-v1.0.0", Expected: releaseDigest}}, Effects: []setupcontract.Effect{
		{ID: "cli", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0", "sha256": cliDigest, "release_manifest_digest": releaseDigest, "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest}},
		{ID: "record", Kind: "platform_installation", Subject: layout.Root, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": releaseDigest, "platform_setup_contract_json": `{}`, "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest}},
	}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	raw, _ := json.Marshal(plan)
	_, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSetupPlan(ctx, store.SetupPlanRecord{PlanID: plan.PlanID, Kind: string(plan.Kind), SchemaVersion: 1, Target: layout.Root, DigestSHA256: digest, CanonicalJSON: string(canonical), Projection: "inspection", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPlatformInstallation(ctx, store.PlatformInstallation{PlatformVersion: "1.0.0", ReleaseManifestDigestSHA256: releaseDigest, PlatformSetupContractDigestSHA256: contractDigest, WorkflowCLISHA256: cliDigest, WorkflowHome: layout.Root, InstalledAt: now, VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runSetupInspectPlatform([]string{"--workflow-home", layout.Root}, &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Status string             `json:"status"`
		Result platformInspection `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status != "ready" || !response.Result.GitHubCredential.Verified || strings.Join(response.Result.GitHubCredential.Scopes, ",") != "repo,workflow" || !response.Result.WorkflowCLI.Verified {
		t.Fatalf("inspection=%s err=%v", output.String(), err)
	}
	if err := os.WriteFile(filepath.Join(layout.Bin, workflowhome.ExecutableName), []byte("drifted-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runSetupInspectPlatform([]string{"--workflow-home", layout.Root}, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status != "blocked" || response.Result.WorkflowCLI.Verified {
		t.Fatalf("drifted CLI inspection=%s err=%v", output.String(), err)
	}
}

func TestReadyShortcutRequiresFullPlatformAndAdmissionVerification(t *testing.T) {
	originalPlatform, originalAdmission := verifyPlatformReadyForSetup, verifyRecordedAdmissionForSetup
	t.Cleanup(func() {
		verifyPlatformReadyForSetup, verifyRecordedAdmissionForSetup = originalPlatform, originalAdmission
	})
	platformCalls, admissionCalls := 0, 0
	verifyPlatformReadyForSetup = func(context.Context, *store.Store, workflowhome.Layout) error {
		platformCalls++
		return errors.New("Docker probe failed")
	}
	verifyRecordedAdmissionForSetup = func(context.Context, *store.Store, workflowhome.Layout, *github.Client, string) error {
		admissionCalls++
		return nil
	}
	err := verifySetupReady(context.Background(), nil, workflowhome.Layout{}, nil, "owner/repo")
	if err == nil || !strings.Contains(err.Error(), "Docker probe failed") || platformCalls != 1 || admissionCalls != 1 {
		t.Fatalf("ready verification err=%v platform=%d admission=%d", err, platformCalls, admissionCalls)
	}
}

func TestStoppedControlPlaneBlocksOnboardingPlanAndDoesNotClaimPlatformReady(t *testing.T) {
	original := verifyPlatformReadyForSetup
	t.Cleanup(func() { verifyPlatformReadyForSetup = original })
	verifyPlatformReadyForSetup = func(context.Context, *store.Store, workflowhome.Layout) error {
		return errors.New("Control Plane is stopped")
	}
	var output bytes.Buffer
	ready, err := requirePlatformReadyForOnboarding(context.Background(), nil, workflowhome.Layout{}, &output)
	if err != nil || ready {
		t.Fatalf("ready=%t err=%v", ready, err)
	}
	var response setupResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status != "blocked" || response.PlatformReady || !strings.Contains(response.Blocker, "stopped") {
		t.Fatalf("response=%s err=%v", output.String(), err)
	}
}

func TestSetupCommandsRequireAbsoluteRepository(t *testing.T) {
	for _, run := range []func([]string, *bytes.Buffer) error{func(args []string, out *bytes.Buffer) error { return runSetupPlan(args, out) }, func(args []string, out *bytes.Buffer) error { return runSetupVerify(args, out) }} {
		var output bytes.Buffer
		if err := run([]string{"--repo", "relative"}, &output); err == nil {
			t.Fatal("relative repository accepted")
		}
	}
}
