package workerrelease

import (
	"strings"
	"testing"
)

const canonicalManifest = `{"schema_version":1,"version":"0.0.1","source_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","github_actions_run_id":123,"bundle":{"name":"workflow-windows-amd64.zip","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"worker":{"image":"ghcr.io/skyhuang233/workflow-worker@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","tools":{"codex":{"version":"0.148.0"},"github_cli":{"version":"2.97.0","linux_amd64_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"go":{"version":"1.26.6","linux_amd64_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},"no_mistakes":{"version":"v1.41.2","repository":"skyhuang233/no-mistakes","commit":"2222222222222222222222222222222222222222"}}},"sbom":{"name":"worker-sbom.spdx.json","format":"spdx-json","sha256":"3333333333333333333333333333333333333333333333333333333333333333","scan":{"scanner":"grype","severity_cutoff":"high","only_fixed":true}}}`

func TestDecodeToolProvenanceReturnsCanonicalWorkerIdentityAndTools(t *testing.T) {
	provenance, err := DecodeToolProvenance([]byte(canonicalManifest))
	if err != nil {
		t.Fatal(err)
	}
	if provenance.Version != "0.0.1" || provenance.SourceCommit != strings.Repeat("a", 40) || provenance.ImageReference != "ghcr.io/skyhuang233/workflow-worker@sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("release provenance = %#v", provenance)
	}
	versions, err := provenance.ToolVersions()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"codex": "0.148.0", "github-cli": "2.97.0", "go": "1.26.6", "no-mistakes": "v1.41.2"}
	for tool, version := range want {
		if versions[tool] != version {
			t.Fatalf("tool versions = %#v, want %s=%s", versions, tool, version)
		}
	}
}

func TestDecodeToolProvenanceRejectsRetiredFlatShapeAndMissingNestedTools(t *testing.T) {
	flat := `{"codex_version":"0.147.0","github_cli_version":"2.97.0","go_version":"1.25.12","no_mistakes_version":"v1.41.2"}`
	if _, err := DecodeToolProvenance([]byte(flat)); err == nil {
		t.Fatal("accepted retired flat provenance")
	}
	missingGitHubCLI := strings.Replace(canonicalManifest, `"github_cli":{"version":"2.97.0","linux_amd64_sha256":"`+strings.Repeat("e", 64)+`"},`, "", 1)
	if _, err := DecodeToolProvenance([]byte(missingGitHubCLI)); err == nil || !strings.Contains(err.Error(), "GitHub CLI") {
		t.Fatalf("missing GitHub CLI error = %v", err)
	}
}
