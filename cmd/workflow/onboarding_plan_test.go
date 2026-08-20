package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/onboarding"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
)

func TestOnboardingPlanAfterMergedContractOnlyReconcilesAdmission(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	generation := "generation"
	generationPath := filepath.Join(home, "platform", "generations", generation)
	if err := os.MkdirAll(generationPath, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, filepath.Join(generationPath, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	token := "pat"
	credentialPath := filepath.Join(home, "state", "credentials", "github.pat")
	if err := credential.NewFileStore(credentialPath).Set(ctx, credential.GatewayTarget, token); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "owner", UserID: 1, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: credentialPath, Status: "verified", VerifiedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	active := launcher.Active{SchemaVersion: 1, Generation: generation, Version: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), Readiness: "ready"}
	activeJSON, _ := json.Marshal(active)
	if err := os.WriteFile(filepath.Join(home, "platform", "active.json"), activeJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	repository := filepath.Join(t.TempDir(), "repo")
	onboardingPlanTestGit(t, "", "init", "-b", "main", repository)
	onboardingPlanTestGit(t, repository, "config", "user.name", "Test")
	onboardingPlanTestGit(t, repository, "config", "user.email", "test@example.com")
	onboardingPlanTestGit(t, repository, "config", "core.autocrlf", "false")
	files, _, _, err := repositorycontract.Render("single-context", nil, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range files {
		target := filepath.Join(repository, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	onboardingPlanTestGit(t, repository, "add", ".")
	onboardingPlanTestGit(t, repository, "commit", "-m", "merge onboarding contract")
	onboardingPlanTestGit(t, repository, "remote", "add", "origin", "https://github.com/owner/repo.git")
	head := strings.TrimSpace(onboardingPlanTestGit(t, repository, "rev-parse", "HEAD"))

	labels := []onboarding.Label{
		{Name: "workflow:inbox", Color: "5319e7", Description: "Agent Workflow inbox"},
		{Name: "workflow:plan", Color: "0e8a16", Description: "Agent Workflow delivery plan"},
		{Name: "workflow:ticket", Color: "1d76db", Description: "Agent Workflow executable ticket"},
		{Name: "workflow:active", Color: "fbca04", Description: "Agent Workflow active work"},
		{Name: "workflow:delivered", Color: "006b75", Description: "Agent Workflow delivered work"},
	}
	labelsByName := map[string]onboarding.Label{}
	for _, label := range labels {
		labelsByName[label.Name] = label
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "owner", "id": 1, "type": "User"})
		case r.URL.Path == "/repos/owner/repo":
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "owner/repo", "default_branch": "main", "has_issues": true, "permissions": map[string]bool{"admin": true}, "allow_squash_merge": true})
		case r.URL.Path == "/repos/owner/repo/git/ref/heads/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": head}})
		case r.URL.Path == "/repos/owner/repo/actions/permissions":
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "allowed_actions": "all"})
		case r.URL.Path == "/repos/owner/repo/branches/main/protection":
			http.NotFound(w, r)
		case r.URL.Path == "/repos/owner/repo/rulesets":
			_, _ = w.Write([]byte("[]"))
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/labels/"):
			name := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/labels/")
			label, ok := labelsByName[name]
			if !ok {
				t.Fatalf("unexpected label read: %s", name)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"name": label.Name, "color": label.Color, "description": label.Description})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/contents/"):
			path := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/contents/")
			data, ok := files[path]
			if !ok {
				t.Fatalf("unexpected managed content read: %s", path)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString(data), "encoding": "base64"})
		default:
			t.Fatalf("unexpected onboarding plan request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	if err := onboardingCommand([]string{"plan", "--workflow-home", home, "--repo", repository, "--github-api", server.URL}, bytes.NewReader(nil), &output); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OnboardingPlan setupcontract.Plan `json:"onboarding_plan"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	effects := envelope.OnboardingPlan.Effects
	if len(effects) != 1 || effects[0].Kind != "repository_admission" {
		t.Fatalf("post-merge Onboarding Plan effects = %#v, want admission only", effects)
	}
	var plannedLabels []onboarding.Label
	if err := json.Unmarshal([]byte(effects[0].Parameters["labels_json"]), &plannedLabels); err != nil {
		t.Fatal(err)
	}
	if len(plannedLabels) != len(labels) {
		t.Fatalf("admission labels = %#v, want all required Workflow labels", plannedLabels)
	}
}

func onboardingPlanTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
