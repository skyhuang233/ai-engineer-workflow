package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubapp"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type githubTokenProviderFunc func(context.Context) (string, error)

func (f githubTokenProviderFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

func TestProvisionGitHubAppDiscoversInstallationVerifiesContractAndStoresOnlyIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	permissions := map[string]string{
		"actions": "read", "checks": "read", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write",
	}
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/integration/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "repository_selection": "all", "account": map[string]string{"login": "owner"}})
		case "/app/installations/42/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "installation_token", "expires_at": time.Now().UTC().Add(time.Hour), "permissions": permissions})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := doctor.Config{GitHub: doctor.GitHubPin{
		TestRepository: "owner/integration",
		Credential:     doctor.GitHubCredentialPin{Owner: "owner", AllRepositories: true, Permissions: permissions},
	}}
	verified := false
	err = provisionGitHubApp(ctx, db, config, 123, privateKeyPEM, githubAppProvisionDependencies{
		APIBase: server.URL, Client: server.Client(),
		Verify: func(_ context.Context, token, owner, repository string) error {
			verified = token == "installation_token" && owner == "owner" && repository == "owner/integration"
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("live contract did not receive the installation token and configured repository identity")
	}
	verification, err := db.GitHubAppVerification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if verification.AppID != 123 || verification.InstallationID != 42 || verification.FingerprintSHA256 != privateKeyFingerprint(privateKeyPEM) {
		t.Fatalf("GitHub App verification = %#v", verification)
	}
	if paused, _, err := db.GatewayWritesPaused(ctx); err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
	joined := strings.Join(paths, "\n")
	for _, want := range []string{"GET /repos/owner/integration/installation", "POST /app/installations/42/access_tokens"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in GitHub App calls:\n%s", want, joined)
		}
	}
	t.Logf("provision discovered the owner-wide installation and minted its token before the live contract; Gateway writes resumed.\nGitHub API transcript:\n%s\nPersisted verification: app_id=%d installation_id=%d pem_sha256=%s owner=%s repository=%s verified_at=%s",
		joined, verification.AppID, verification.InstallationID, verification.FingerprintSHA256,
		verification.Owner, verification.IntegrationRepository, verification.VerifiedAt.UTC().Format(time.RFC3339Nano))
}

func TestProvisionGitHubAppPausesWritesBeforeReadingPrivateKey(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	config := doctor.Config{GitHub: doctor.GitHubPin{
		TestRepository: "owner/integration",
		Credential: doctor.GitHubCredentialPin{
			Owner: "owner", PrivateKeyFile: filepath.Join(t.TempDir(), "missing.pem"),
			Permissions: map[string]string{"metadata": "read"},
		},
	}}
	err = provisionGitHubApp(ctx, db, config, 123, nil, githubAppProvisionDependencies{})
	if err == nil || !strings.Contains(err.Error(), "read GitHub App private key") {
		t.Fatalf("provision missing private key error = %v", err)
	}
	paused, _, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || !paused {
		t.Fatalf("Gateway writes paused before private-key read = %t, %v", paused, pauseErr)
	}
}

func TestVerifiedGitHubAppTokenSourceReloadsProvisionedInstallation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyFile := filepath.Join(t.TempDir(), "github-app.pem")
	firstKey := testGitHubAppPrivateKeyPEM(t)
	secondKey := testGitHubAppPrivateKeyPEM(t)
	if err := os.WriteFile(keyFile, firstKey, 0o600); err != nil {
		t.Fatal(err)
	}
	permissions := map[string]string{"metadata": "read"}
	var liveInstallationMu sync.RWMutex
	liveInstallationID := int64(42)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/integration/installation":
			liveInstallationMu.RLock()
			installationID := liveInstallationID
			liveInstallationMu.RUnlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": installationID, "repository_selection": "all", "account": map[string]string{"login": "owner"}})
		case "/app/installations/42/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "first_token", "expires_at": time.Now().UTC().Add(time.Hour), "permissions": permissions})
		case "/app/installations/84/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "second_token", "expires_at": time.Now().UTC().Add(time.Hour), "permissions": permissions})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := doctor.Config{GitHub: doctor.GitHubPin{TestRepository: "owner/integration", Credential: doctor.GitHubCredentialPin{
		Owner: "owner", PrivateKeyFile: keyFile, Permissions: permissions,
	}}}
	record := func(appID, installationID int64, key []byte) {
		t.Helper()
		liveInstallationMu.Lock()
		liveInstallationID = installationID
		liveInstallationMu.Unlock()
		if err := db.RecordGitHubAppVerification(ctx, store.GitHubAppVerification{
			FingerprintSHA256: privateKeyFingerprint(key), AppID: appID, InstallationID: installationID,
			Owner: "owner", IntegrationRepository: "owner/integration", VerifiedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(123, 42, firstKey)
	source := &verifiedGitHubAppTokenSource{Database: db, Config: config, APIBase: server.URL, Client: server.Client()}
	if token, err := source.Token(ctx); err != nil || token != "first_token" {
		t.Fatalf("first installation token = %q, %v", token, err)
	}
	if err := os.WriteFile(keyFile, secondKey, 0o600); err != nil {
		t.Fatal(err)
	}
	record(246, 84, secondKey)
	if token, err := source.Token(ctx); err != nil || token != "second_token" {
		t.Fatalf("rotated installation token = %q, %v", token, err)
	}
	t.Log("the long-running token source hot-loaded the reprovisioned App identity, PEM fingerprint, and installation (app 123/installation 42 -> app 246/installation 84) without restart")
}

func TestVerifiedGitHubAppTokenSourceReloadsSameIdentityAfterReprovision(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyFile := filepath.Join(t.TempDir(), "github-app.pem")
	privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	if err := os.WriteFile(keyFile, privateKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	permissions := map[string]string{"metadata": "read"}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/integration/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "repository_selection": "all", "account": map[string]string{"login": "owner"}})
		case "/app/installations/42/access_tokens":
			requests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": fmt.Sprintf("token_%d", requests), "expires_at": time.Now().UTC().Add(time.Hour), "permissions": permissions,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := doctor.Config{GitHub: doctor.GitHubPin{TestRepository: "owner/integration", Credential: doctor.GitHubCredentialPin{
		Owner: "owner", PrivateKeyFile: keyFile, Permissions: permissions,
	}}}
	verifiedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	record := func(at time.Time) {
		t.Helper()
		if err := db.RecordGitHubAppVerification(ctx, store.GitHubAppVerification{
			FingerprintSHA256: privateKeyFingerprint(privateKeyPEM), AppID: 123, InstallationID: 42,
			Owner: "OWNER", IntegrationRepository: "OWNER/INTEGRATION", VerifiedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(verifiedAt)
	source := &verifiedGitHubAppTokenSource{Database: db, Config: config, APIBase: server.URL, Client: server.Client()}
	if token, err := source.Token(ctx); err != nil || token != "token_1" {
		t.Fatalf("initial installation token = %q, %v", token, err)
	}
	record(verifiedAt.Add(time.Second))
	if token, err := source.Token(ctx); err != nil || token != "token_2" || requests != 2 {
		t.Fatalf("reprovisioned installation token = %q, requests=%d, err=%v", token, requests, err)
	}
}

func TestVerifiedGitHubAppTokenSourceCachesLiveSelectionAndRejectsDriftAfterTTL(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyFile := filepath.Join(t.TempDir(), "github-app.pem")
	privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	if err := os.WriteFile(keyFile, privateKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var selectionMu sync.RWMutex
	selection := "all"
	now := time.Now().UTC()
	installationRequests := 0
	tokenRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/integration/installation":
			installationRequests++
			selectionMu.RLock()
			liveSelection := selection
			selectionMu.RUnlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "repository_selection": liveSelection, "account": map[string]string{"login": "owner"}})
		case "/app/installations/42/access_tokens":
			tokenRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "cached_token", "expires_at": time.Now().UTC().Add(time.Hour), "permissions": map[string]string{"metadata": "read"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := doctor.Config{GitHub: doctor.GitHubPin{TestRepository: "owner/integration", Credential: doctor.GitHubCredentialPin{
		Owner: "owner", PrivateKeyFile: keyFile, Permissions: map[string]string{"metadata": "read"},
	}}}
	if err := db.RecordGitHubAppVerification(ctx, store.GitHubAppVerification{
		FingerprintSHA256: privateKeyFingerprint(privateKeyPEM), AppID: 123, InstallationID: 42,
		Owner: "owner", IntegrationRepository: "owner/integration", VerifiedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	source := &verifiedGitHubAppTokenSource{Database: db, Config: config, APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return now }}
	if token, err := source.Token(ctx); err != nil || token != "cached_token" {
		t.Fatalf("initial installation token = %q, %v", token, err)
	}
	selectionMu.Lock()
	selection = "selected"
	selectionMu.Unlock()
	if token, err := source.Token(ctx); err != nil || token != "cached_token" {
		t.Fatalf("installation verification cache token = %q, err=%v", token, err)
	}
	if installationRequests != 1 {
		t.Fatalf("live installation requests inside cache TTL = %d, want 1", installationRequests)
	}
	now = now.Add(liveInstallationVerificationTTL + time.Second)
	if token, err := source.Token(ctx); token != "" || !errors.Is(err, githubapp.ErrCredentialUnavailable) {
		t.Fatalf("selection drift token = %q, err=%v", token, err)
	}
	if installationRequests != 2 {
		t.Fatalf("live installation requests after cache TTL = %d, want 2", installationRequests)
	}
	if tokenRequests != 1 {
		t.Fatalf("installation token requests = %d, want cached token blocked before refresh", tokenRequests)
	}
}

func TestLoadVerifiedGitHubAppProviderRejectsLiveDriftBeforeTokenUse(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyFile := filepath.Join(t.TempDir(), "github-app.pem")
	privateKeyPEM := testGitHubAppPrivateKeyPEM(t)
	if err := os.WriteFile(keyFile, privateKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/repos/owner/integration/installation" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "repository_selection": "selected", "account": map[string]string{"login": "owner"}})
	}))
	defer server.Close()
	config := doctor.Config{GitHub: doctor.GitHubPin{TestRepository: "owner/integration", Credential: doctor.GitHubCredentialPin{
		Owner: "owner", PrivateKeyFile: keyFile, Permissions: map[string]string{"metadata": "read"},
	}}}
	if err := db.RecordGitHubAppVerification(ctx, store.GitHubAppVerification{
		FingerprintSHA256: privateKeyFingerprint(privateKeyPEM), AppID: 123, InstallationID: 42,
		Owner: "owner", IntegrationRepository: "owner/integration", VerifiedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = loadVerifiedGitHubAppProvider(ctx, db, config, server.URL, server.Client())
	if !errors.Is(err, githubapp.ErrCredentialUnavailable) {
		t.Fatalf("live installation drift error = %v", err)
	}
	if len(paths) != 1 || paths[0] != "/repos/owner/integration/installation" {
		t.Fatalf("GitHub calls before drift rejection = %#v", paths)
	}
	if err := persistGitHubAppAdmissionError(ctx, db, err, time.Now().UTC()); !errors.Is(err, githubapp.ErrCredentialUnavailable) {
		t.Fatalf("persist live drift pause = %v", err)
	}
	paused, reason, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || !paused || reason != store.ControlPlaneGitHubAppRecoveryRemediation {
		t.Fatalf("live drift pause = %t, %q, %v", paused, reason, pauseErr)
	}
}

func testGitHubAppPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestDefaultCodexAuthFileFollowsCodexHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", home)
	if got, want := defaultCodexAuthFile(), filepath.Join(home, "auth.json"); got != want {
		t.Fatalf("defaultCodexAuthFile() = %q, want %q", got, want)
	}
	t.Logf("workflow commands defaulted --codex-auth-file to %s", filepath.Join(home, "auth.json"))
}

func TestDoctorVerificationBudgetAllowsColdWorkerPullAndCodexResume(t *testing.T) {
	if doctorVerificationTimeout != 10*time.Minute {
		t.Fatalf("doctorVerificationTimeout = %s, want 10m", doctorVerificationTimeout)
	}
	t.Logf("workflow doctor verification budget = %s", doctorVerificationTimeout)
}

func TestImplementationPromptCarriesPersistedTicketContract(t *testing.T) {
	claim := store.TicketClaim{TicketNumber: 8, TicketTitle: "Add the alpha record"}
	body := "Create qualification/issue20-e2e.md with exactly:\nalpha: issue-20-production-e2e"
	prompt := implementationPrompt(claim, body)
	for _, want := range []string{
		"Implement Executable Ticket #8: Add the alpha record",
		body,
		"Do not call GitHub",
		"Commit all changes and leave the Ticket Workspace clean.",
		"exact full lowercase 40-character Git commit SHA",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("implementationPrompt() omitted %q:\n%s", want, prompt)
		}
	}
}

func TestResolveWorkerPromptUsesImmutableBodyForInitialRun(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Body: "authoritative acceptance criteria", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	claim := store.TicketClaim{VersionID: version.ID, TicketID: 1, TicketNumber: 11, TicketTitle: "first"}
	prompt, err := resolveWorkerPrompt(ctx, db, claim, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, snapshot.Children[0].Body) {
		t.Fatalf("initial Worker prompt = %q", prompt)
	}
}

func TestResolveWorkerPromptPreservesReviewRevisionExactly(t *testing.T) {
	persisted := "  Review feedback ID review/50:\nkeep surrounding whitespace exactly  "
	prompt, err := resolveWorkerPrompt(context.Background(), nil, store.TicketClaim{}, persisted)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != persisted {
		t.Fatalf("review revision prompt = %q, want %q", prompt, persisted)
	}
}

func TestRecoverInboxDeliveryCLIListsAndAuthorizesOldestGeneration(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repository := "owner/repository"
	snapshot := plan.Snapshot{
		Repository: repository,
		Root:       plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 2, Number: 2, Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour)
	queued, err := db.QueueWorkflowInboxProjection(ctx, repository, now)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	deliveryKey := queued.IdempotencyKey
	for attempt := 0; attempt < 8; attempt++ {
		claim, claimErr := db.ClaimDeliveryOutbox(ctx, deliveryKey, now)
		if claimErr != nil {
			db.Close()
			t.Fatalf("claim attempt %d: %v", attempt+1, claimErr)
		}
		if err := db.RequeueDeliveryOutboxClaim(ctx, deliveryKey, claim.ClaimToken, "remote observation unavailable", true, now); err != nil {
			db.Close()
			t.Fatal(err)
		}
		outbox, err := db.DeliveryOutbox(ctx, deliveryKey)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if outbox.State == store.OutboxRejected {
			break
		}
		now = now.Add(time.Minute)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	binaryPath := filepath.Join(t.TempDir(), "workflow-test.exe")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workflow CLI: %v\n%s", err, output)
	}
	list := exec.Command(binaryPath, "recover-inbox-delivery", "--database", databasePath, "--repository", repository)
	output, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("list recoverable Inbox deliveries: %v\n%s", err, output)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[0] != deliveryKey {
		t.Fatalf("recovery listing = %q, want delivery key and question id", output)
	}
	questionID := fields[1]
	t.Logf("$ workflow recover-inbox-delivery --repository %s", repository)
	t.Logf("%s %s", deliveryKey, questionID)

	recoverCommand := exec.Command(binaryPath, "recover-inbox-delivery", "--database", databasePath, "--repository", repository, "--delivery", deliveryKey, "--question", questionID, "--answer", "retry")
	if output, err := recoverCommand.CombinedOutput(); err != nil {
		t.Fatalf("authorize Inbox recovery: %v\n%s", err, output)
	}
	t.Logf("$ workflow recover-inbox-delivery --repository %s --delivery %s --question %s --answer retry", repository, deliveryKey, questionID)

	db, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recovered, err := db.DeliveryOutbox(ctx, deliveryKey)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != store.OutboxPending || !recovered.Uncertain || recovered.Attempts != 0 {
		t.Fatalf("authorized recovery = %#v", recovered)
	}
	keys, err := db.DueDeliveryOutboxKeys(ctx, time.Now().UTC().Add(time.Minute), 10)
	if err != nil || len(keys) < 2 || keys[0] != deliveryKey {
		t.Fatalf("ordered recovery queue = %v, %v", keys, err)
	}
	currentGenerations := make([]int64, 0, len(keys)-1)
	for _, key := range keys[1:] {
		current, err := db.DeliveryOutbox(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if current.Request.InboxProjectionGeneration <= recovered.Request.InboxProjectionGeneration {
			t.Fatalf("ordered generations = %d then %d", recovered.Request.InboxProjectionGeneration, current.Request.InboxProjectionGeneration)
		}
		currentGenerations = append(currentGenerations, current.Request.InboxProjectionGeneration)
	}
	t.Logf("authorized generation %d returned to the queue before current generations %v", recovered.Request.InboxProjectionGeneration, currentGenerations)
}

func TestShouldPauseGatewayForCredential(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "missing", err: fmt.Errorf("%w: private key file is missing", delivery.ErrGatewayCredentialRejected), want: true},
		{name: "rejected", err: fmt.Errorf("%w: fingerprint mismatch", delivery.ErrGatewayCredentialRejected), want: true},
		{name: "live installation unavailable", err: fmt.Errorf("%w: repository selection drift", githubapp.ErrCredentialUnavailable), want: true},
		{name: "transient store error", err: errors.New("database temporarily unavailable")},
		{name: "cancelled", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldPauseGatewayForCredential(test.err); got != test.want {
				t.Fatalf("should pause = %t, want %t", got, test.want)
			}
		})
	}
}

func TestGitHubAppAdmissionsNormalizeAndPersistMissingInstallation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	missing := &githubapp.APIError{Operation: "create GitHub App installation token", StatusCode: http.StatusNotFound}
	provider := githubTokenProviderFunc(func(context.Context) (string, error) { return "", missing })
	_, err = admitPollGitHubCredential(ctx, github.Poller{Store: db}, provider, "owner/repo", nil)
	if !errors.Is(err, delivery.ErrGatewayCredentialRejected) || !errors.Is(err, githubapp.ErrCredentialUnavailable) {
		t.Fatalf("poll-github missing installation error = %v", err)
	}
	_, err = admitControlPlaneGitHubApp(ctx, db, provider, nil)
	if !errors.Is(err, delivery.ErrGatewayCredentialRejected) || !errors.Is(err, githubapp.ErrCredentialUnavailable) {
		t.Fatalf("Gateway missing installation error = %v", err)
	}
	paused, _, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, pauseErr)
	}
}

func TestShouldLogNeedsAttentionErrorWithInboxRecoveryCommand(t *testing.T) {
	plain := fmt.Errorf("poll exhausted: %w", store.ErrNeedsAttention)
	if shouldLogNeedsAttentionError(plain) {
		t.Fatal("plain Needs Attention error should remain suppressed")
	}
	actionable := errors.Join(plain, errors.New("workflow recover-inbox-delivery --repository owner/repo --delivery inbox-key"))
	if !shouldLogNeedsAttentionError(actionable) {
		t.Fatal("uncertain Inbox recovery command should be logged")
	}
}

func TestGatewayControlURLUsesHostOverrideAndPreservesLegacyFallback(t *testing.T) {
	workerURL := "http://host.docker.internal:8787"
	if got := gatewayControlURL(workerURL, ""); got != workerURL {
		t.Fatalf("legacy Gateway control URL = %q, want %q", got, workerURL)
	}
	controlURL := "http://127.0.0.1:8787"
	if got := gatewayControlURL(workerURL, controlURL); got != controlURL {
		t.Fatalf("host Gateway control URL = %q, want %q", got, controlURL)
	}
}

func TestGatewayControlProjectorSendsHostInboxProjectionToOverride(t *testing.T) {
	const controlToken = "control-token"
	requests := 0
	controlGateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/deliveries" {
			t.Errorf("control projection path = %q, want /v1/deliveries", request.URL.Path)
		}
		if got := request.Header.Get("X-Workflow-Control-Token"); got != controlToken {
			t.Errorf("control projection token = %q, want %q", got, controlToken)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer controlGateway.Close()

	projector := gatewayControlProjector("http://host.docker.internal:8787", controlGateway.URL, controlToken)
	if err := projector.ProjectWorkflowInbox(context.Background(), "owner/repo", nil); err != nil {
		t.Fatalf("project host inbox through control Gateway: %v", err)
	}
	if requests != 1 {
		t.Fatalf("control Gateway inbox requests = %d, want 1", requests)
	}
	t.Logf("Inbox projection reached the host control Gateway at %s while Worker routing remains %s", controlGateway.URL, "http://host.docker.internal:8787")
}

func TestMissingGitHubAppVerificationIsRejected(t *testing.T) {
	err := githubAppVerificationError(store.ErrNotFound)
	if !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("missing verification error = %v, want rejected credential", err)
	}
	if !shouldPauseGatewayForCredential(err) {
		t.Fatal("missing verification credential error did not pause Gateway writes")
	}
}

func TestGitHubAppVerificationReadFailureIsRetryable(t *testing.T) {
	err := githubAppVerificationError(errors.New("database temporarily unavailable"))
	if errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("verification read failure = %v, want retryable error", err)
	}
	if shouldPauseGatewayForCredential(err) {
		t.Fatal("verification read failure paused Gateway writes")
	}
}

func TestPersistGitHubAppPauseCreatesLocalRecoveryInbox(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	credentialErr := fmt.Errorf("%w: private key file is missing", delivery.ErrGatewayCredentialRejected)
	if err := persistGitHubAppPause(ctx, db, credentialErr, now); !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("credential pause error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
	inbox, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey)
	if err != nil || inbox.State != "open" || inbox.Title != store.ControlPlaneGitHubAppRecoveryTitle || inbox.Body != store.ControlPlaneGitHubAppRecoveryRemediation {
		t.Fatalf("credential recovery inbox = %#v, %v", inbox, err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 1 {
		t.Fatalf("credential recovery questions = %#v, %v", questions, err)
	}
}

func TestPersistGitHubAppPauseLeavesTransientFailuresRetryable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	transient := errors.New("Credential Manager temporarily unavailable")
	if err := persistGitHubAppPause(ctx, db, transient, time.Now().UTC()); !errors.Is(err, transient) {
		t.Fatalf("transient credential error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
}

func TestPersistGitHubAppAdmissionErrorPausesForRejectedCredential(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	pollErr := fmt.Errorf("repository admission: %w", &github.APIError{StatusCode: http.StatusUnauthorized})
	if err := persistGitHubAppAdmissionError(ctx, db, pollErr, now); !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		t.Fatalf("poll credential error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || !paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
	inbox, err := db.WorkflowInboxItem(ctx, store.GatewayCredentialInboxKey)
	if err != nil || inbox.State != "open" {
		t.Fatalf("credential recovery inbox = %#v, %v", inbox, err)
	}
}

func TestPersistGitHubAppAdmissionErrorLeavesRateLimitsRetryable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	retryAt := time.Date(2026, 8, 4, 0, 1, 0, 0, time.UTC)
	pollErr := &github.APIError{StatusCode: http.StatusForbidden, RetryAt: retryAt}
	if err := persistGitHubAppAdmissionError(ctx, db, pollErr, time.Now().UTC()); !errors.Is(err, pollErr) {
		t.Fatalf("rate limited poll error = %v", err)
	}
	paused, _, err := db.GatewayWritesPaused(ctx)
	if err != nil || paused {
		t.Fatalf("Gateway writes paused = %t, %v", paused, err)
	}
}

func TestCredentialAdmissionConsumesBootstrapWithoutTerminalizingWorkers(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "missing credential", err: fmt.Errorf("%w: private key file is missing", delivery.ErrGatewayCredentialRejected)},
		{name: "rejected by GitHub", err: &github.APIError{StatusCode: http.StatusUnauthorized}},
		{name: "missing installation", err: &githubapp.APIError{Operation: "create GitHub App installation token", StatusCode: http.StatusNotFound}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "workflow.db")
			db, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			repository := "owner/repo"
			now := time.Now().UTC()
			snapshot := plan.Snapshot{
				Repository: repository,
				Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
				Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
			}
			fingerprint, err := snapshot.Fingerprint()
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.MarkActive(ctx, version.ID); err != nil {
				db.Close()
				t.Fatal(err)
			}
			claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.RecordGitHubPollFailureWithKind(ctx, repository, now, store.GitHubPollFailurePreActivationInboxConflict); err != nil {
				db.Close()
				t.Fatal(err)
			}
			admissionErr := persistGitHubAppAdmissionError(ctx, db, test.err, now.Add(time.Second))
			err = recordPollAdmissionFailure(ctx, github.Poller{Store: db, MaxFailures: 5, Now: func() time.Time { return now.Add(time.Second) }}, repository, admissionErr)
			if !errors.Is(err, test.err) && !errors.Is(err, delivery.ErrGatewayCredentialRejected) {
				db.Close()
				t.Fatalf("credential admission failure = %v", err)
			}
			paused, _, pauseErr := db.GatewayWritesPaused(ctx)
			cursor, cursorErr := db.GitHubPollCursor(ctx, repository)
			current, claimErr := db.CurrentClaim(ctx, version.ID, claim.TicketID)
			if pauseErr != nil || !paused || cursorErr != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed || claimErr != nil || current.RunID != claim.RunID {
				db.Close()
				t.Fatalf("credential state paused=%t cursor=%#v claim=%#v errors=%v/%v/%v", paused, cursor, current, pauseErr, cursorErr, claimErr)
			}
			questions, questionErr := db.OpenWorkflowQuestions(ctx, repository, 0)
			if questionErr != nil || len(questions) != 1 || questions[0].Kind != "gateway_credential" {
				db.Close()
				t.Fatalf("credential questions = %#v, %v", questions, questionErr)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			cursor, err = restarted.GitHubPollCursor(ctx, repository)
			if err != nil || cursor.NeedsAttention() || cursor.ConsecutiveFailures != 0 || cursor.FailureKind != store.GitHubPollFailureRetryable || cursor.RecoveryState != store.GitHubPollRecoveryConsumed {
				t.Fatalf("restarted credential cursor = %#v, %v", cursor, err)
			}
		})
	}
}

func TestPollAdmissionHonorsNextAttemptBeforeCredentialAccess(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := "owner/repo"
	now := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	if err := db.DeferGitHubPoll(ctx, repository, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	authenticated := false
	_, err = admitPollGitHubCredential(ctx, github.Poller{Store: db, Now: func() time.Time { return now }}, nil, repository, func(string) error {
		authenticated = true
		return nil
	})
	if !errors.Is(err, store.ErrNotReady) || authenticated {
		t.Fatalf("deferred admission error=%v authenticated=%t", err, authenticated)
	}
	paused, _, pauseErr := db.GatewayWritesPaused(ctx)
	if pauseErr != nil || paused {
		t.Fatalf("deferred admission paused=%t error=%v", paused, pauseErr)
	}
}

func TestRequireOwnerGuardedControlPlaneRepositoryAcceptsPrivateRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"owner/repo","owner":{"login":"owner"},"private":true}`))
	}))
	defer server.Close()
	err := requireOwnerGuardedControlPlaneRepository(context.Background(), github.NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner"), "owner/repo")
	if err != nil {
		t.Fatalf("private repository admission error = %v", err)
	}
}

func TestAcquireTicketClaimReplacesExpiredWorker(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now.Add(-2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: expired.SessionID, AgentIdentity: "agent-1", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	replacement, prompt, err := acquireTicketClaim(ctx, db, version.ID, expired.TicketID, store.DefaultMaxWorkerAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "" || replacement.SessionID != expired.SessionID || replacement.Attempt != expired.Attempt+1 || replacement.LeaseGeneration != expired.LeaseGeneration+1 {
		t.Fatalf("replacement = %#v, prompt = %q", replacement, prompt)
	}
	t.Logf("run-ticket claim acquisition reused Ticket Session %s and advanced from attempt %d to %d", replacement.SessionID, expired.Attempt, replacement.Attempt)
}

func TestAcquireTicketClaimRestoresClaimedReviewPromptBeforeControllerRun(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Body: "initial specification must not replace review feedback", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeliveryController(ctx, delivery, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	feedbackBody := "  preserve this review feedback exactly  \nincluding its whitespace"
	if _, err := db.RecordReviewFeedback(ctx, version.ID, claim.TicketID, []store.ReviewFeedback{{Source: "review", EventID: "50", Author: "human", Body: feedbackBody}}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	revision, expectedPrompt, err := db.ClaimQueuedReviewRevision(ctx, version.ID, claim.TicketID, time.Hour, now.Add(3*time.Second), 1, store.DefaultMaxWorkerAttempts)
	if err != nil {
		t.Fatal(err)
	}
	recovered, recoveredPrompt, err := acquireTicketClaim(ctx, db, version.ID, claim.TicketID, store.DefaultMaxWorkerAttempts, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RunID != revision.RunID || recoveredPrompt != expectedPrompt {
		t.Fatalf("recovered claim = %#v, prompt = %q; want run %q, prompt %q", recovered, recoveredPrompt, revision.RunID, expectedPrompt)
	}
	resolved, err := resolveWorkerPrompt(ctx, db, recovered, recoveredPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != expectedPrompt || strings.Contains(resolved, snapshot.Children[0].Body) {
		t.Fatalf("resolved recovery prompt = %q, want exact claimed review prompt %q", resolved, expectedPrompt)
	}
}

func TestDispatchPendingDeliveryClaimsOnlyLaunchesRecoveredDelivery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailDeliveryController(ctx, delivery, "delivery failed", now.Add(time.Second)); err != nil {
		t.Fatalf("failed delivery = %v", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, snapshot.Repository, snapshot.Root.Number)
	if err != nil || len(questions) != 1 {
		t.Fatalf("questions = %#v, %v", questions, err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, snapshot.Repository, questions[0].ID, "retry", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	launched := make(chan store.TicketClaim, 1)
	if err := dispatchPendingDeliveryClaims(ctx, db, snapshot.Repository, 1, time.Hour, now.Add(2*time.Second), func(_ context.Context, retry store.TicketClaim) error {
		launched <- retry
		return nil
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case retry := <-launched:
		if retry.RunID == claim.RunID || retry.RunID == "" {
			t.Fatalf("launched delivery claim = %#v", retry)
		}
	case <-time.After(time.Second):
		t.Fatal("recovered delivery was not launched")
	}
}

func TestLaunchDeliveryClaimsDoesNotBlockControlLoop(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	claims := []store.TicketClaim{{RunID: "delivery-1"}, {RunID: "delivery-2"}}
	if err := launchDeliveryClaims(context.Background(), claims, func(_ context.Context, claim store.TicketClaim) error {
		started <- claim.RunID
		<-release
		return nil
	}, nil, nil); err != nil {
		t.Fatalf("launch delivery claims: %v", err)
	}
	for range claims {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("delivery claims did not launch concurrently")
		}
	}
	close(release)
}

func TestLaunchDeliveryClaimsJoinsOnceModeFailures(t *testing.T) {
	var workers sync.WaitGroup
	observed := make(chan error, 1)
	if err := launchDeliveryClaims(context.Background(), []store.TicketClaim{{RunID: "delivery-1"}}, func(context.Context, store.TicketClaim) error {
		return errors.New("delivery failed")
	}, &workers, func(err error) { observed <- err }); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if err := <-observed; err == nil || err.Error() != "delivery failed" {
		t.Fatalf("observed failure = %v", err)
	}
}
