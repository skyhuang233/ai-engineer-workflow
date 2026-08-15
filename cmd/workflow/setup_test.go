package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/onboarding"
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

func TestSetupVerificationJSONRetainsExactEvidenceAndRepairHintsWithoutPAT(t *testing.T) {
	const token = "ghp_setup_verification_secret"
	report := &setupVerificationReport{}
	report.Credential.setupVerificationCheck = setupVerificationBlocked(errors.New("credential "+token+" drifted"), "repair credential", token)
	report.Credential.Login, report.Credential.UserID, report.Credential.Owner = "alice", 7, "owner"
	report.Credential.Scopes = []string{"repo", "workflow"}
	report.Credential.FingerprintSHA256 = strings.Repeat("a", 64)
	report.Discovery.setupVerificationCheck = setupVerificationCheck{Status: "verified", Evidence: "exact discovery"}
	report.Discovery.Repository, report.Discovery.RepositoryPath, report.Discovery.Origin = "owner/repo", `C:\repo`, "https://github.com/owner/repo.git"
	report.Discovery.DefaultBranch, report.Discovery.Head, report.Discovery.Published = "main", strings.Repeat("b", 40), true
	report.Admission.setupVerificationCheck = setupVerificationCheck{Status: "blocked", Evidence: "manifest drift", RepairHint: "rerun Repository Onboarding"}
	report.Admission.Repository, report.Admission.OnboardingPlanDigestSHA256 = "owner/repo", strings.Repeat("c", 64)
	report.Admission.ContractVersion, report.Admission.ManifestDigestSHA256 = "1", strings.Repeat("d", 64)
	report.Readiness.setupVerificationCheck = setupVerificationCheck{Status: "blocked", Evidence: "repository is not ready", RepairHint: "follow stage hints"}
	report.Readiness.PlatformReady = true

	var output bytes.Buffer
	if err := writeSetupResponse(&output, setupResponse{Status: "blocked", PlatformReady: true, Verification: report}); err != nil {
		t.Fatal(err)
	}
	encoded := output.String()
	for _, required := range []string{`"login":"alice"`, `"user_id":7`, `"owner":"owner"`, `"fingerprint_sha256":"` + strings.Repeat("a", 64), `"repository":"owner/repo"`, `"default_branch":"main"`, `"head":"` + strings.Repeat("b", 40), `"onboarding_plan_digest_sha256":"` + strings.Repeat("c", 64), `"manifest_digest_sha256":"` + strings.Repeat("d", 64), `"repair_hint":"repair credential"`, `"repair_hint":"rerun Repository Onboarding"`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("verification JSON omitted %q: %s", required, encoded)
		}
	}
	if strings.Contains(encoded, token) || !strings.Contains(encoded, "[redacted]") {
		t.Fatalf("verification JSON leaked PAT-shaped input: %s", encoded)
	}
}

func TestSetupPlanBindsConfirmedPublicationStateBeforePlatformReadback(t *testing.T) {
	repository := t.TempDir()
	command := exec.Command("git", "-C", repository, "init")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	var output bytes.Buffer
	if err := runSetupPlan([]string{"--repo", repository, "--publication-state", "published", "--repository-name", "repo", "--visibility", "private", "--domain-layout", "single-context"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "confirmed publication state") {
		t.Fatalf("setup plan did not fence confirmed publication intent: %s", output.String())
	}
}

func TestPlatformStateSnapshotChangesForAnotherValidPATOrCodexSource(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	record := func(fingerprint string) {
		t.Helper()
		if err := database.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: fingerprint, Login: "owner", UserID: 7, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	record(strings.Repeat("a", 64))
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"one"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := resolveCodexAuthForSetup
	resolveCodexAuthForSetup = func(context.Context) (string, error) { return auth, nil }
	t.Cleanup(func() { resolveCodexAuthForSetup = previous })
	approved, err := currentPlatformStateDigest(ctx, database, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"two"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changedCodex, err := currentPlatformStateDigest(ctx, database, true)
	if err != nil || changedCodex == approved {
		t.Fatalf("changed Codex snapshot=%q err=%v", changedCodex, err)
	}
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"one"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record(strings.Repeat("b", 64))
	changedPAT, err := currentPlatformStateDigest(ctx, database, true)
	if err != nil || changedPAT == approved {
		t.Fatalf("changed PAT snapshot=%q err=%v", changedPAT, err)
	}
}

func TestSetupPlanReportsOldSchemaRepairBlockerWithoutMigrating(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(layout.State, "workflow.db")
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version,applied_at) VALUES(59,'old')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	beforeTree := setupPlanTreeSnapshot(t, layout.Root)
	var output bytes.Buffer
	if err := runSetupPlan([]string{"--repo", t.TempDir(), "--workflow-home", layout.Root}, &output); err != nil {
		t.Fatal(err)
	}
	var response setupResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status != "blocked" || !strings.Contains(response.Blocker, "approved Platform repair") {
		t.Fatalf("old schema response=%s err=%v", output.String(), err)
	}
	afterTree := setupPlanTreeSnapshot(t, layout.Root)
	if !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatalf("setup plan changed Workflow Home tree:\nbefore=%#v\nafter=%#v", beforeTree, afterTree)
	}
	raw, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 59 {
		t.Fatalf("setup plan migrated schema: version=%d err=%v", version, err)
	}
}

func TestSetupInspectPlatformReportsOldSchemaRepairBlockerWithoutMigrating(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(layout.State, "workflow.db")
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version,applied_at) VALUES(59,'old')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	beforeTree := setupPlanTreeSnapshot(t, layout.Root)
	var output bytes.Buffer
	if err := runSetupInspectPlatform([]string{"--workflow-home", layout.Root}, &output); err != nil {
		t.Fatal(err)
	}
	var response setupResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status != "blocked" || !strings.Contains(response.Blocker, "approved Platform repair") {
		t.Fatalf("old schema inspection response=%s err=%v", output.String(), err)
	}
	afterTree := setupPlanTreeSnapshot(t, layout.Root)
	if !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatalf("setup inspect-platform changed Workflow Home tree:\nbefore=%#v\nafter=%#v", beforeTree, afterTree)
	}
	raw, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 59 {
		t.Fatalf("setup inspect-platform migrated schema: version=%d err=%v", version, err)
	}
}

type setupPlanTreeEntry struct {
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
}

func setupPlanTreeSnapshot(t *testing.T, root string) map[string]setupPlanTreeEntry {
	t.Helper()
	result := map[string]setupPlanTreeEntry{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = setupPlanTreeEntry{Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSetupPlanAdmissionInspectionDoesNotRefreshOrSuspendStoredAdmission(t *testing.T) {
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
	verifiedAt := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	want := store.RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: strings.Repeat("a", 64), ContractVersion: "1", ManifestDigestSHA256: strings.Repeat("b", 64), Eligible: true, VerifiedAt: verifiedAt}
	if err := database.RecordRepositoryAdmission(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordedAdmissionReadOnly(context.Background(), database, layout, nil, want.Repository); err == nil {
		t.Fatal("missing installed contract unexpectedly verified")
	}
	got, err := database.RepositoryAdmission(context.Background(), want.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Eligible || got.SuspensionReason != "" || !got.VerifiedAt.Equal(verifiedAt) {
		t.Fatalf("setup plan mutated admission: %#v", got)
	}
}

func TestInspectPlatformLiveValidatesPersistedPATWithoutSecretInput(t *testing.T) {
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account","id_token":"id","refresh_token":"refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previousResolver := resolveCodexAuthForSetup
	resolveCodexAuthForSetup = func(context.Context) (string, error) { return auth, nil }
	t.Cleanup(func() { resolveCodexAuthForSetup = previousResolver })
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
	bundleJSON, bundleDigest := testReleaseBundle(cliDigest)
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "platform-inspection", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: layout.Root}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: "platform-v1.0.0", Expected: releaseDigest}}, Effects: []setupcontract.Effect{
		{ID: "cli", Kind: "platform_cli", Subject: filepath.Join(layout.Bin, workflowhome.ExecutableName), Action: "install", Parameters: map[string]string{"version": "1.0.0", "sha256": cliDigest, "release_manifest_digest": releaseDigest, "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_digest": bundleDigest}},
		{ID: "record", Kind: "platform_installation", Subject: layout.Root, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": releaseDigest, "platform_setup_contract_json": `{}`, "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_json": bundleJSON, "release_bundled_files_digest": bundleDigest}},
	}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}}}
	raw, _ := json.Marshal(plan)
	_, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSetupPlan(ctx, store.SetupPlanRecord{PlanID: plan.PlanID, Kind: string(plan.Kind), SchemaVersion: 1, Target: layout.Root, DigestSHA256: digest, CanonicalJSON: string(canonical), Projection: "inspection", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	cpDigest := strings.Repeat("e", 64)
	if err := db.RecordPlatformInstallation(ctx, store.PlatformInstallation{PlatformVersion: "1.0.0", ReleaseManifestDigestSHA256: releaseDigest, PlatformSetupContractDigestSHA256: contractDigest, WorkflowCLISHA256: cliDigest, ReleaseBundledFilesJSON: bundleJSON, ReleaseBundledFilesDigestSHA256: bundleDigest, ControlPlanePlanDigestSHA256: cpDigest, WorkflowHome: layout.Root, InstalledAt: now, VerifiedAt: now}); err != nil {
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
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Status != "ready" || response.Result.Platform.WorkflowCLISHA256 != cliDigest || response.Result.Platform.ControlPlanePlanDigest != cpDigest || !response.Result.GitHubCredential.Verified || response.Result.GitHubCredential.FingerprintSHA256 != credential.Fingerprint(token) || strings.Join(response.Result.GitHubCredential.Scopes, ",") != "repo,workflow" || !response.Result.WorkflowCLI.Verified || !response.Result.CodexAuth.Verified || response.Result.CodexAuth.Source != auth {
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

func testReleaseBundle(cliDigest string) (string, string) {
	raw, _ := json.Marshal([]map[string]string{{"path": "bin/workflow.exe", "sha256": cliDigest}})
	canonical, digest, _ := setupcontract.Canonicalize(raw)
	return string(canonical), digest
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

func TestReadyShortcutRejectsConfirmedRepositoryIntentDrift(t *testing.T) {
	baseDiscovery := onboarding.Discovery{Repository: "owner/repo", Origin: "https://github.com/owner/repo.git", DefaultBranch: "main", Published: true}
	basePolicy := onboarding.RepositoryPolicy{Private: true}
	baseManifest := repositorycontract.Manifest{Repository: "owner/repo", DefaultBranch: "main", DomainLayout: "single-context"}
	if err := validateConfirmedReadyIntent(baseDiscovery, basePolicy, baseManifest, "owner", "repo", true, "single-context"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		discovery  onboarding.Discovery
		policy     onboarding.RepositoryPolicy
		manifest   repositorycontract.Manifest
		repository string
		private    bool
		layout     string
		want       string
	}{
		{name: "missing confirmed name", discovery: baseDiscovery, policy: basePolicy, manifest: baseManifest, private: true, layout: "single-context", want: "not confirmed"},
		{name: "name", discovery: baseDiscovery, policy: basePolicy, manifest: baseManifest, repository: "other", private: true, layout: "single-context", want: "identity"},
		{name: "origin", discovery: onboarding.Discovery{Repository: "owner/repo", Origin: "https://github.com/owner/repo", DefaultBranch: "main"}, policy: basePolicy, manifest: baseManifest, repository: "repo", private: true, layout: "single-context", want: "canonical"},
		{name: "visibility", discovery: baseDiscovery, policy: onboarding.RepositoryPolicy{Private: false}, manifest: baseManifest, repository: "repo", private: true, layout: "single-context", want: "visibility"},
		{name: "domain layout", discovery: baseDiscovery, policy: basePolicy, manifest: baseManifest, repository: "repo", private: true, layout: "multi-context", want: "domain layout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateConfirmedReadyIntent(test.discovery, test.policy, test.manifest, "owner", test.repository, test.private, test.layout)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("confirmed drift accepted: %v", err)
			}
		})
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

func TestSetupRemoteAcceptsOnlyCanonicalGitHubOrigin(t *testing.T) {
	for _, accepted := range []string{"https://github.com/owner/repo", "https://github.com/owner/repo.git", "git@github.com:owner/repo", "git@github.com:owner/repo.git"} {
		if got, err := parseOriginRepository(accepted); err != nil || got != "owner/repo" {
			t.Errorf("canonical origin %q parsed as %q, %v", accepted, got, err)
		}
	}
	for _, rejected := range []string{
		"http://github.com/owner/repo.git",
		"https://user@github.com/owner/repo.git",
		"https://github.com:443/owner/repo.git",
		"https://github.com/owner/repo.git?token=secret",
		"https://github.com/owner/repo.git#fragment",
		"ssh://git@github.com/owner/repo.git",
		"other@github.com:owner/repo.git",
		"git@github.com:22/owner/repo.git",
	} {
		if got, err := parseOriginRepository(rejected); err == nil {
			t.Errorf("noncanonical origin %q accepted as %q", rejected, got)
		}
	}
}
