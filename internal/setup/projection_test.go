package setup

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func TestProjectShowsExactManagedFileContentsAndRedactsSecretInputs(t *testing.T) {
	files, _ := json.Marshal(map[string]string{"docs/agents/domain.md": base64.StdEncoding.EncodeToString([]byte("# Domain\nexact bytes\n"))})
	plan := setupcontract.Plan{SchemaVersion: 1, PlanID: "plan", Kind: setupcontract.RepositoryOnboarding, Target: setupcontract.Target{WorkflowHome: `C:\Workflow`, RepositoryPath: `C:\repo`}, Effects: []setupcontract.Effect{
		{ID: "files", Kind: "repository_contract_pr", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"files_json": string(files)}},
		{ID: "pat", Kind: "github_pat", Subject: `C:\Workflow\github.pat`, Action: "persist", Parameters: map[string]string{"input": "stdin", "secret_token": "must-not-appear"}},
	}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "repository_admission", Subject: "owner/repo", Expected: strings.Repeat("a", 64)}}}
	projection := Project(plan, strings.Repeat("b", 64))
	for _, expected := range []string{"--- docs/agents/domain.md", "+ # Domain", "+ exact bytes", "secret_token: <redacted>"} {
		if !strings.Contains(projection, expected) {
			t.Fatalf("projection lacks %q:\n%s", expected, projection)
		}
	}
	if strings.Contains(projection, "must-not-appear") {
		t.Fatal("projection leaked secret")
	}
}
