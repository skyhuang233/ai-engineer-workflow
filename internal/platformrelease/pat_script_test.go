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

func TestVerifyGitHubPATRunsOnWindowsPowerShell51AndBindsPersonalOrOrganizationOwner(t *testing.T) {
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
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
			_, _ = w.Write([]byte(`{"login":"alice","id":7}`))
		case "/orgs/acme/memberships/alice":
			_, _ = w.Write([]byte(`{"state":"active","role":"admin"}`))
		case "/orgs/sso-blocked/memberships/alice":
			w.Header().Set("X-GitHub-SSO", "required; url=https://github.com/orgs/sso-blocked/sso")
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, current, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts", "verify-github-pat.ps1")
	run := func(owner string) ([]byte, error) {
		command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-APIBase", server.URL, "-Owner", owner)
		command.Stdin = strings.NewReader(token)
		return command.CombinedOutput()
	}
	for _, owner := range []string{"alice", "acme"} {
		output, runErr := run(owner)
		var result struct {
			Login       string `json:"login"`
			Owner       string `json:"owner"`
			Fingerprint string `json:"fingerprint_sha256"`
			UserID      int64  `json:"user_id"`
		}
		clean := bytes.TrimPrefix(output, []byte{0xef, 0xbb, 0xbf})
		if decodeErr := json.Unmarshal(clean, &result); runErr != nil || decodeErr != nil {
			t.Fatalf("owner %s: output=%q run=%v decode=%v", owner, output, runErr, decodeErr)
		}
		sum := sha256.Sum256([]byte(token))
		if result.Login != "alice" || result.Owner != owner || result.UserID != 7 || result.Fingerprint != hex.EncodeToString(sum[:]) {
			t.Fatalf("owner %s result=%#v", owner, result)
		}
	}
	output, runErr := run("sso-blocked")
	if runErr == nil || !strings.Contains(string(output), "SSO") {
		t.Fatalf("SSO-blocked organization was not rejected: %q, %v", output, runErr)
	}
}

func TestBootstrapSkillDeterminesOwnerAndReleaseBeforePlatformPlanning(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	skillPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, required := range []string{"present its owner as the candidate", "With no `origin`, explicitly ask", "before Platform planning", "-Owner <owner>", "exact release identified by its durable version and manifest digest", "only when the user explicitly requested an upgrade", "-AllowUpgrade"} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks owner/release decision contract %q", required)
		}
	}
}
