package workerrelease

import (
	"strings"
	"testing"
)

func TestDecodeToolProvenanceReturnsCompleteToolVersions(t *testing.T) {
	provenance, err := DecodeToolProvenance([]byte(`{"codex_version":"0.147.0","github_cli_version":"2.97.0","go_version":"1.25.12","no_mistakes_version":"v1.41.2"}`))
	if err != nil {
		t.Fatal(err)
	}
	versions, err := provenance.ToolVersions()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"codex": "0.147.0", "github-cli": "2.97.0", "go": "1.25.12", "no-mistakes": "v1.41.2"}
	for tool, version := range want {
		if versions[tool] != version {
			t.Fatalf("tool versions = %#v, want %s=%s", versions, tool, version)
		}
	}
}

func TestDecodeToolProvenanceRejectsMissingToolVersions(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		missing string
	}{
		{name: "Codex", json: `{"github_cli_version":"2.97.0","go_version":"1.25.12","no_mistakes_version":"v1.41.2"}`, missing: "Codex"},
		{name: "GitHub CLI", json: `{"codex_version":"0.147.0","go_version":"1.25.12","no_mistakes_version":"v1.41.2"}`, missing: "GitHub CLI"},
		{name: "Go", json: `{"codex_version":"0.147.0","github_cli_version":"2.97.0","no_mistakes_version":"v1.41.2"}`, missing: "Go"},
		{name: "no-mistakes", json: `{"codex_version":"0.147.0","github_cli_version":"2.97.0","go_version":"1.25.12"}`, missing: "no-mistakes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeToolProvenance([]byte(test.json)); err == nil || !strings.Contains(err.Error(), test.missing) {
				t.Fatalf("missing %s error = %v", test.missing, err)
			}
		})
	}
}
