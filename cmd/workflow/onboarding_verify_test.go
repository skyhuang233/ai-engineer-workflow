package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOnboardingVerifyRechecksExactAdmittedContract(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	gen := "g"
	os.MkdirAll(filepath.Join(home, "platform", "generations", gen), 0o700)
	if err := os.MkdirAll(filepath.Join(home, "platform", "generations", gen), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(home, "platform", "generations", gen, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	token := "pat"
	cred := filepath.Join(home, "state", "credentials", "github.pat")
	if err = credential.NewFileStore(cred).Set(ctx, credential.GatewayTarget, token); err != nil {
		t.Fatal(err)
	}
	if err = db.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "owner", UserID: 1, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: cred, Status: "verified", VerifiedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	files, _, manifest, err := repositorycontract.Render("single-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "onboard-verify", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: home, RepositoryPath: t.TempDir(), GitHubRepository: "owner/repo"}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "platform_release", Subject: home, Expected: strings.Repeat("a", 64)}, {ID: "head", Kind: "git_head", Subject: filepath.Join(home, "repo"), Expected: ""}}, Effects: []setupcontract.Effect{{ID: "a", Kind: "repository_admission", Subject: "owner/repo", Action: "verify_and_record", Parameters: map[string]string{"default_branch": "main", "manifest_digest": manifest, "contract_version": "1"}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: manifest}}}
	raw, _ := json.Marshal(plan)
	_, canon, digest, _ := setupcontract.ParsePlan(raw)
	if err = db.RecordSetupPlan(ctx, store.SetupPlanRecord{PlanID: "p", Kind: string(plan.Kind), SchemaVersion: 1, Target: plan.Target.RepositoryPath, DigestSHA256: digest, CanonicalJSON: string(canon), Projection: "owner/repo", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Now()
	if err = db.RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx,
		store.RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: digest, ContractVersion: "1", ManifestDigestSHA256: manifest, Eligible: true, VerifiedAt: admittedAt},
		store.RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", SourcePath: plan.Target.RepositoryPath, GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: 7 * 24 * time.Hour, MaxParallelRuns: 1, UpdatedAt: admittedAt},
	); err != nil {
		t.Fatal(err)
	}
	contentReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
			json.NewEncoder(w).Encode(map[string]any{"login": "owner", "id": 1, "type": "User"})
			return
		}
		for p, b := range files {
			if r.URL.Path == "/repos/owner/repo/contents/"+p {
				contentReads++
				json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString(b), "encoding": "base64"})
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	a := launcher.Active{SchemaVersion: 1, Generation: gen, Version: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), Readiness: "ready"}
	data, _ := json.Marshal(a)
	os.MkdirAll(filepath.Join(home, "platform"), 0700)
	os.WriteFile(filepath.Join(home, "platform", "active.json"), data, 0600)
	var out bytes.Buffer
	wrongRepositoryPath := t.TempDir()
	if err = onboardingCommand([]string{"apply", "--workflow-home", home, "--repo", wrongRepositoryPath, "--github-api", server.URL, "--onboarding-plan-digest", digest}, bytes.NewReader(nil), &out); err == nil || !strings.Contains(err.Error(), "differs from the approved Onboarding Plan") {
		t.Fatalf("apply did not resume the stored approved Onboarding Plan before checking its repository path: %v", err)
	}
	if err = onboardingCommand([]string{"apply", "--workflow-home", home, "--repo", wrongRepositoryPath, "--github-api", server.URL, "--onboarding-plan-digest", digest}, bytes.NewReader(canon), &out); err == nil || !strings.Contains(err.Error(), "differs from the approved Onboarding Plan") {
		t.Fatalf("apply accepted repository path outside the approved Onboarding Plan: %v", err)
	}
	if contentReads != 0 {
		t.Fatal("repository path mismatch reached remote apply effects")
	}
	if err = onboardingCommand([]string{"verify", "--workflow-home", home, "--repo", wrongRepositoryPath, "--github-api", server.URL, "--onboarding-plan-digest", digest}, bytes.NewReader(nil), &out); err == nil || !strings.Contains(err.Error(), "differs from the approved Onboarding Plan") {
		t.Fatalf("verify accepted repository path outside the approved Onboarding Plan: %v", err)
	}
	if contentReads != 0 {
		t.Fatal("repository path mismatch reached remote contract readback")
	}
	if err = onboardingCommand([]string{"verify", "--workflow-home", home, "--repo", plan.Target.RepositoryPath, "--github-api", server.URL, "--onboarding-plan-digest", digest}, bytes.NewReader(nil), &out); err != nil {
		t.Fatal(err)
	}
	for path := range files {
		if path != repositorycontract.ManifestPath {
			files[path] = []byte("remote managed drift\n")
			break
		}
	}
	if err = onboardingCommand([]string{"verify", "--workflow-home", home, "--repo", plan.Target.RepositoryPath, "--github-api", server.URL, "--onboarding-plan-digest", digest}, bytes.NewReader(nil), &out); err == nil {
		t.Fatal("accepted remote managed-file drift")
	}
	readsBeforePATFailure := contentReads
	if err := credential.NewFileStore(cred).Set(ctx, credential.GatewayTarget, "replaced-pat"); err != nil {
		t.Fatal(err)
	}
	if err = onboardingCommand([]string{"verify", "--workflow-home", home, "--repo", plan.Target.RepositoryPath, "--github-api", server.URL, "--onboarding-plan-digest", digest}, bytes.NewReader(nil), &out); err == nil {
		t.Fatal("accepted replaced PAT")
	}
	if contentReads != readsBeforePATFailure {
		t.Fatal("PAT failure reached remote contract readback")
	}
}
