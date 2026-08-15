package setup

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func TestProjectShowsExactManagedFileContentsAndRedactsSecretInputs(t *testing.T) {
	files, _ := json.Marshal(map[string]string{"docs/agents/domain.md": base64.StdEncoding.EncodeToString([]byte("# Domain\nnew bytes\n")), "created.md": base64.StdEncoding.EncodeToString([]byte("created\n"))})
	before, _ := json.Marshal(map[string]string{"docs/agents/domain.md": base64.StdEncoding.EncodeToString([]byte("# Domain\nold bytes\n")), "deleted.md": base64.StdEncoding.EncodeToString([]byte("deleted\n"))})
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "plan", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: `C:\Workflow`, RepositoryPath: `C:\repo`}, Effects: []setupcontract.Effect{
		{ID: "files", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"before_files_json": string(before), "files_json": string(files)}},
		{ID: "pat", Kind: "github_pat", Subject: `C:\Workflow\github.pat`, Action: "persist", Parameters: map[string]string{"input": "stdin", "secret_token": "must-not-appear"}},
	}, Preconditions: []setupcontract.Precondition{{ID: "head", Kind: "git_head", Subject: `C:\repo`, Expected: "abc123"}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: strings.Repeat("a", 64)}}}
	projection := Project(plan, strings.Repeat("b", 64))
	for _, expected := range []string{"Preconditions:", "head: C:\\repo (git_head) = abc123", "--- docs/agents/domain.md", "- old bytes", "+ new bytes", "--- /dev/null", "+++ created.md", "--- deleted.md", "+++ /dev/null", "repository_admission", "secret_token: <redacted>"} {
		if !strings.Contains(projection, expected) {
			t.Fatalf("projection lacks %q:\n%s", expected, projection)
		}
	}
	if strings.Contains(projection, "must-not-appear") {
		t.Fatal("projection leaked secret")
	}
}

func TestProjectShowsExactApprovedPersonalPATScopes(t *testing.T) {
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "personal-pat", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: `C:\Workflow`}, Effects: []setupcontract.Effect{{ID: "pat", Kind: "github_pat", Subject: `C:\Workflow\state\credentials\github.pat`, Action: "persist", Parameters: map[string]string{"input": "stdin", "owner": "alice", "required_scopes": "repo,workflow"}}}}
	projection := Project(plan, strings.Repeat("a", 64))
	if !strings.Contains(projection, "required_scopes: repo,workflow") || strings.Contains(projection, "admin:org") {
		t.Fatalf("projection does not show only the approved personal PAT scopes:\n%s", projection)
	}
}

func TestProjectFileDiffMakesTrailingNewlineAndBinaryBytesUnambiguous(t *testing.T) {
	before, _ := json.Marshal(map[string]string{
		"newline.txt": base64.StdEncoding.EncodeToString([]byte("x\n")),
		"binary.bin":  base64.StdEncoding.EncodeToString([]byte{0, 1, 255}),
	})
	after, _ := json.Marshal(map[string]string{
		"newline.txt": base64.StdEncoding.EncodeToString([]byte("x")),
		"binary.bin":  base64.StdEncoding.EncodeToString([]byte{0, 2, 255}),
	})
	projection, ok := projectFileDiff(string(before), string(after))
	if !ok {
		t.Fatal("byte-exact file projection failed")
	}
	for _, expected := range []string{`bytes=2`, `text_json="x\n"`, `bytes=1`, `text_json="x"`, `encoding=base64`, `base64=AAH/`, `base64=AAL/`} {
		if !strings.Contains(projection, expected) {
			t.Fatalf("projection lacks %q:\n%s", expected, projection)
		}
	}
	if count := len(regexp.MustCompile(`sha256=[0-9a-f]{64}`).FindAllString(projection, -1)); count != 4 {
		t.Fatalf("projection has %d byte digests, want 4:\n%s", count, projection)
	}
}
