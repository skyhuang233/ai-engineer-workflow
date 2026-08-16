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
		case "/repos/alice/owner-guarded":
			_, _ = w.Write([]byte(`{"full_name":"alice/owner-guarded","private":true,"default_branch":"main","allow_squash_merge":true,"permissions":{"admin":true}}`))
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
		case "/repos/alice/owner-guarded/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"all"}`))
		case "/repos/alice/owner-guarded/rulesets", "/repos/alice/owner-guarded/branches/main/protection":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"GitHub plan does not support this private repository policy"}`))
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
	if output, runErr = run("alice", "owner-guarded", "published"); runErr != nil {
		t.Fatalf("private Owner-Guarded repository with unavailable optional policy was rejected: %q, %v", output, runErr)
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
	for _, required := range []string{"present its owner as the candidate together with its repository name", "With no `origin`, explicitly ask", "repository name and private/public visibility", "before any Platform mutation", "confirmed owner, repository name, visibility, publication state, and domain layout", "-Owner <owner> -RepositoryName <name> -Visibility <private|public> -PublicationState <published|unpublished>", "workflow setup plan --workflow-home $confirmedWorkflowHome --repo (Get-Location).Path --repository-name <confirmed-name> --visibility <private|public> --publication-state <published|unpublished> --domain-layout <single-context|multi-context>", "workflow setup verify --workflow-home $confirmedWorkflowHome --repo (Get-Location).Path", "plan target.workflow_home exactly equals `$confirmedWorkflowHome`", "scripts/resolve-platform-release.ps1", "HostFactsPath = $hostFactsPath", "$resolvedRelease.manifest_path", "fresh host may either use the default latest stable release", "On a fresh host, add Version alone", "AllowUpgrade only when an installed lower version", "AllowUpgrade = $true", "verified backup pin automatically supplies exact repair authority", "omit `Version` for that exact pin repair", "both verified pins are missing while the Workflow CLI exists", "confirm the exact installed version", "$releaseArguments.Version = <confirmed-exact-installed-version>", "never use latest-stable selection for a pinless existing installation", "contract-validated, forward-only dry run"} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks owner/release decision contract %q", required)
		}
	}
	for _, required := range []string{
		"ask once for the classic PAT",
		"even when `$hostFacts.github_credential` is already live-verified",
		"current Setup invocation's memory",
		"skip only the Control Plane credential verifier",
		"Control Plane credential verifier",
		"Release Resolver",
		"exact-package installer",
		"BOM-free copy of the same in-memory PAT",
		"clear `$setupPAT` before stopping",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks single-PAT setup contract %q", required)
		}
	}
	if strings.Contains(content, "For every resolution, ask for the user's PAT explicitly") {
		t.Fatal("bootstrap skill must not require users to re-enter the PAT for release resolution")
	}
	firstCapture := strings.Index(content, "ask once for the classic PAT")
	resolver := strings.Index(content, "Reuse the PAT already captured for this Setup invocation")
	installer := strings.Index(content, "do not ask for the PAT again")
	if firstCapture < 0 || resolver < firstCapture || installer < resolver {
		t.Fatalf("bootstrap skill must capture once before reuse by resolver and installer: capture=%d resolver=%d installer=%d", firstCapture, resolver, installer)
	}
	for _, obsolete := range []string{"obtain the exact release", "SignaturePath", "signature_path", ".sig", "pinned public key", "missing pinned key"} {
		if strings.Contains(content, obsolete) {
			t.Fatalf("bootstrap skill retains obsolete signing instruction %q", obsolete)
		}
	}
	if strings.Contains(content, "with `-ManifestPath`, `-SignaturePath`") {
		t.Fatal("bootstrap skill still asks the agent to obtain or manually supply release trust inputs")
	}
}

func TestBootstrapSkillRestartsStoppedControlPlaneWithDurableAuthorization(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	skillPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, required := range []string{
		"existing-authorization Control Plane restart",
		`$hostFacts.workflow.trust_state -eq "pinned"`,
		`$hostFacts.workflow.owned`,
		`$hostFacts.platform.installation_recorded`,
		`$hostFacts.platform.control_plane_plan_digest_sha256`,
		`$hostFacts.control_plane.state -eq "stopped"`,
		`$confirmedWorkflowHome = [IO.Path]::GetFullPath(<confirmed-absolute-local-workflow-home>)`,
		`serve --workflow-home $confirmedWorkflowHome`,
		`workflow.exe serve --workflow-home`,
		`--approved-plan-digest $hostFacts.platform.control_plane_plan_digest_sha256`,
		"must not produce a new Platform Bootstrap Plan or ask for a new approval",
		"Incomplete or conflicting durable trust fails closed",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks stopped Control Plane authorization contract %q", required)
		}
	}
}

func TestBootstrapSkillRestartsOnlyWhenControlPlaneProcessAbsenceIsProven(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	skillPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, required := range []string{
		`$durableAuthorityExact`,
		`$staleNonLiveRecord`,
		`$stoppedWithoutRecord`,
		`$restartEligible`,
		`$hostFacts.control_plane.state -eq "stale"`,
		`$hostFacts.control_plane.state -eq "stopped"`,
		`$null -eq $hostFacts.control_plane.runtime`,
		`[string]$hostFacts.workflow.version -ceq [string]$hostFacts.platform.version`,
		`[string]$hostFacts.workflow.sha256 -ceq [string]$hostFacts.platform.workflow_cli_sha256`,
		`[string]$hostFacts.control_plane.runtime.platform_version -ceq [string]$hostFacts.platform.version`,
		`[string]$hostFacts.control_plane.runtime.approved_platform_bootstrap_plan_digest_sha256 -ceq [string]$hostFacts.platform.control_plane_plan_digest_sha256`,
		"Never restart `stopped` with a Runtime Record",
		"`mismatched`",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks process-absence restart contract %q", required)
		}
	}
	for _, field := range []string{"release_manifest_digest", "platform_setup_contract_digest", "workflow_cli_sha256", "release_bundled_files_digest", "control_plane_plan_digest_sha256"} {
		if !strings.Contains(content, `$hostFacts.platform.`+field+` -cmatch $sha256Pattern`) {
			t.Fatalf("bootstrap skill restart does not require exact durable pin %q", field)
		}
	}
}

func TestHostInspectionDoesNotDescribeStatusFailureAsNoRuntimeRecord(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	scriptPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts", "inspect-host.ps1")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	want := `[ordered]@{ state = "mismatched"; diagnostic = "installed Workflow CLI status command failed" }`
	if !strings.Contains(content, want) {
		t.Fatalf("inspect-host must fail closed when Control Plane status cannot prove process absence")
	}
}

func TestBootstrapSkillChoosesWorkflowHomeBeforeFirstInspection(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	skillPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, required := range []string{
		"Confirm the absolute local Workflow Home once before the first host inspection",
		`inspect-host.ps1 -Repository (Get-Location) -WorkflowHome $confirmedWorkflowHome`,
		"Compare both Workflow Home paths by filesystem identity",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks first-inspection Workflow Home contract %q", required)
		}
	}
}

func TestBootstrapSkillObtainsGitConsentBeforeFullHostInspection(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	skillPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	probe := `inspect-host.ps1 -Repository (Get-Location) -WorkflowHome $confirmedWorkflowHome -GitProbeOnly`
	full := `inspect-host.ps1 -Repository (Get-Location) -WorkflowHome $confirmedWorkflowHome | Out-String`
	doctor := "codex doctor --json"
	probeIndex, doctorIndex, fullIndex := strings.Index(content, probe), strings.Index(content, doctor), strings.Index(content, full)
	if probeIndex < 0 || doctorIndex < 0 || fullIndex < 0 || probeIndex >= doctorIndex || doctorIndex >= fullIndex {
		t.Fatalf("bootstrap skill must gate Codex and full host inspection behind GitProbeOnly: probe=%d doctor=%d full=%d", probeIndex, doctorIndex, fullIndex)
	}
	for _, required := range []string{
		"Stop immediately if Git is unavailable",
		"ask once whether to run `git init`",
		"Stop immediately if declined",
		"rerun the Git-only probe",
		"Do not run full host inspection, Docker inspection, Control Plane inspection, release resolution, or `workflow.exe serve` until the Git-only probe confirms a repository",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap skill lacks pre-platform Git gate %q", required)
		}
	}
}

func TestHostInspectionBindsEveryDurablePlatformFieldToVerifiedPin(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	scriptPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts", "inspect-host.ps1")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, field := range []string{"version", "release_manifest_digest", "platform_setup_contract_digest", "workflow_cli_sha256", "release_bundled_files_json", "release_bundled_files_digest", "control_plane_plan_digest_sha256"} {
		needle := `[string]$inspectedPlatform.` + field + ` -cne [string]$platform.` + field
		if !strings.Contains(content, needle) {
			t.Fatalf("inspect-host does not bind signed pin field %q before trusting durable state", field)
		}
	}
}
