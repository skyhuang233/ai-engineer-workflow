package platformrelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerifyGitHubPATRunsOnWindowsPowerShell51AndFailsClosedForUnapprovedOrganizationScope(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is the supported bootstrap shell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	token := "ghp_windows51"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, workflow, admin:org")
			_, _ = w.Write([]byte(`{"login":"alice","id":7}`))
		case "/orgs/acme/memberships/alice":
			_, _ = w.Write([]byte(`{"state":"active","role":"admin"}`))
		case "/repos/alice/repo", "/repos/acme/repo":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case "/repos/alice/published":
			_, _ = w.Write([]byte(`{"full_name":"alice/published","private":true,"default_branch":"main","has_issues":true,"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":false,"permissions":{"admin":true}}`))
		case "/repos/alice/read-only":
			_, _ = w.Write([]byte(`{"full_name":"alice/read-only","private":true,"permissions":{"admin":false}}`))
		case "/repos/alice/no-merge":
			_, _ = w.Write([]byte(`{"full_name":"alice/no-merge","private":true,"default_branch":"main","allow_squash_merge":false,"allow_merge_commit":false,"allow_rebase_merge":false,"permissions":{"admin":true}}`))
		case "/repos/alice/protected":
			_, _ = w.Write([]byte(`{"full_name":"alice/protected","private":true,"default_branch":"main","allow_squash_merge":true,"permissions":{"admin":true}}`))
		case "/repos/alice/published/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":false,"allowed_actions":"selected"}`))
		case "/repos/alice/published/actions/permissions/selected-actions":
			_, _ = w.Write([]byte(`{"github_owned_allowed":true}`))
		case "/repos/alice/published/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/alice/published/branches/main/protection":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not protected"}`))
		case "/repos/alice/protected/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"all"}`))
		case "/repos/alice/protected/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/alice/protected/branches/main/protection":
			_, _ = w.Write([]byte(`{"required_pull_request_reviews":{"required_approving_review_count":1}}`))
		case "/orgs/acme":
			_, _ = w.Write([]byte(`{"login":"acme"}`))
		case "/orgs/acme/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled_repositories":"all","allowed_actions":"all"}`))
		case "/orgs/acme/rulesets":
			_, _ = w.Write([]byte(`[]`))
		case "/orgs/sso-blocked/memberships/alice":
			w.Header().Set("X-GitHub-SSO", "required; url=https://github.com/orgs/sso-blocked/sso")
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, current, _, _ := runtime.Caller(0)
	productionScript := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts", "verify-github-pat.ps1")
	productionBody, err := os.ReadFile(productionScript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(productionBody), "$APIBase") || !strings.Contains(string(productionBody), `"https://api.github.com`) {
		t.Fatal("production PAT verifier must use the fixed public GitHub API origin without an APIBase override")
	}
	script := copyPATVerifierForTest(t, productionScript, server.URL)
	run := func(owner, repository, publicationState string) ([]byte, error) {
		command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Owner", owner, "-RepositoryName", repository, "-Visibility", "private", "-PublicationState", publicationState)
		command.Stdin = strings.NewReader(token)
		return command.CombinedOutput()
	}
	for _, owner := range []string{"alice"} {
		output, runErr := run(owner, "repo", "unpublished")
		var result struct {
			Login       string `json:"login"`
			Owner       string `json:"owner"`
			OwnerType   string `json:"owner_type"`
			Fingerprint string `json:"fingerprint_sha256"`
			UserID      int64  `json:"user_id"`
		}
		clean := bytes.TrimPrefix(output, []byte{0xef, 0xbb, 0xbf})
		if decodeErr := json.Unmarshal(clean, &result); runErr != nil || decodeErr != nil {
			t.Fatalf("owner %s: output=%q run=%v decode=%v", owner, output, runErr, decodeErr)
		}
		sum := sha256.Sum256([]byte(token))
		wantOwnerType := "personal"
		if owner != "alice" {
			wantOwnerType = "organization"
		}
		if result.Login != "alice" || result.Owner != owner || result.OwnerType != wantOwnerType || result.UserID != 7 || result.Fingerprint != hex.EncodeToString(sum[:]) {
			t.Fatalf("owner %s result=%#v", owner, result)
		}
	}
	output, runErr := run("acme", "repo", "unpublished")
	if runErr == nil || !strings.Contains(strings.ToLower(string(output)), "approved organization scope contract") {
		t.Fatalf("organization setup without an approved scope contract was not blocked: %q, %v", output, runErr)
	}
	if output, runErr = run("alice", "published", "published"); runErr != nil {
		t.Fatalf("published repository with administration permission was rejected: %q, %v", output, runErr)
	}
	output, runErr = run("alice", "read-only", "published")
	if runErr == nil || !strings.Contains(string(output), "repository administration") {
		t.Fatalf("published repository without administration permission was not rejected: %q, %v", output, runErr)
	}
	output, runErr = run("alice", "no-merge", "published")
	if runErr == nil || !strings.Contains(string(output), "supported merge method") {
		t.Fatalf("published repository without an onboarding merge method was not rejected: %q, %v", output, runErr)
	}
	output, runErr = run("alice", "protected", "published")
	if runErr == nil || !strings.Contains(string(output), "requires human review") {
		t.Fatalf("published repository with an unfulfillable review policy was not rejected: %q, %v", output, runErr)
	}
}

func copyPATVerifierForTest(t *testing.T, productionScript, apiBase string) string {
	t.Helper()
	sourceRoot := filepath.Dir(productionScript)
	targetRoot := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"verify-github-pat.ps1", "resolve-github-required-scopes.ps1"} {
		body, err := os.ReadFile(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "verify-github-pat.ps1" {
			const fixedAPIBase = "https://api.github.com"
			if strings.Count(string(body), fixedAPIBase) != 1 {
				t.Fatalf("production PAT verifier fixed API origin count = %d, want 1", strings.Count(string(body), fixedAPIBase))
			}
			body = []byte(strings.Replace(string(body), fixedAPIBase, apiBase, 1))
		}
		if err := os.WriteFile(filepath.Join(targetRoot, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(targetRoot, "verify-github-pat.ps1")
}

func TestBootstrapSkillDeterminesOwnerAndReleaseBeforePlatformPlanning(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	skillPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, required := range []string{"present its owner as the candidate together with its repository name", "With no `origin`, explicitly ask", "repository name and private/public visibility", "before any Platform mutation", "confirmed owner, repository name, visibility, publication state, and domain layout", "-Owner <owner> -RepositoryName <name> -Visibility <private|public> -PublicationState <published|unpublished>", "workflow setup plan --repo (Get-Location).Path --repository-name <confirmed-name> --visibility <private|public> --publication-state <published|unpublished> --domain-layout <single-context|multi-context>", "scripts/resolve-platform-release.ps1", "HostFactsPath = $hostFactsPath", "$resolvedRelease.manifest_path", "$resolvedRelease.signature_path", "fresh host may either use the default latest stable release", "On a fresh host, add Version alone", "AllowUpgrade only when an installed lower version", "AllowUpgrade = $true"} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks owner/release decision contract %q", required)
		}
	}
	if strings.Contains(content, "obtain the exact release") || strings.Contains(content, "with `-ManifestPath`, `-SignaturePath`") {
		t.Fatal("bootstrap skill still asks the agent to obtain or manually supply release trust inputs")
	}
}
