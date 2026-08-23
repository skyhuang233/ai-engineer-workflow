package workflowrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestGoldenCorpus(t *testing.T) {
	validDirectory := filepath.Join("testdata", "manifest", "valid")
	validEntries, err := os.ReadDir(validDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(validEntries) == 0 {
		t.Fatal("manifest corpus has no valid fixtures")
	}
	for _, entry := range validEntries {
		t.Run("valid/"+entry.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(validDirectory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeManifest(raw); err != nil {
				t.Fatalf("valid golden manifest: %v", err)
			}
		})
	}
	invalidDirectory := filepath.Join("testdata", "manifest", "invalid")
	invalidEntries, err := os.ReadDir(invalidDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidEntries) == 0 {
		t.Fatal("manifest corpus has no invalid fixtures")
	}
	for _, entry := range invalidEntries {
		t.Run("invalid/"+entry.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(invalidDirectory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeManifest(raw); err == nil {
				t.Fatal("invalid golden manifest was accepted")
			}
		})
	}
}

func validManifestJSON() string {
	return `{"schema_version":1,"version":"0.0.1","candidate_source_commit":"` + strings.Repeat("a", 40) + `","qualification_run_id":42,"qualification_run_attempt":3,"bundle":{"name":"workflow-windows-amd64.zip","sha256":"` + strings.Repeat("b", 64) + `"},"worker":{"image":"ghcr.io/skyhuang233/workflow-worker@sha256:` + strings.Repeat("c", 64) + `","tools":{"codex":{"version":"0.148.0"},"github_cli":{"version":"2.97.0","linux_amd64_sha256":"` + strings.Repeat("e", 64) + `"},"go":{"version":"1.26.6","linux_amd64_sha256":"` + strings.Repeat("f", 64) + `"},"no_mistakes":{"version":"v1.41.2","repository":"skyhuang233/no-mistakes","commit":"` + strings.Repeat("2", 40) + `"}}},"sbom":{"name":"worker-sbom.spdx.json","format":"spdx-json","sha256":"` + strings.Repeat("4", 64) + `","scan":{"scanner":"grype","severity_cutoff":"high","only_fixed":true}}}`
}

func TestDecodeManifestAcceptsTheAtomicWorkflowReleaseContract(t *testing.T) {
	manifest, err := DecodeManifest([]byte(validManifestJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "0.0.1" || manifest.Bundle.Name != BundleAssetName || manifest.SBOM.Name != SBOMAssetName {
		t.Fatalf("manifest = %#v", manifest)
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != validManifestJSON() {
		t.Fatalf("canonical manifest differs\ngot:  %s\nwant: %s", canonical, validManifestJSON())
	}
}

func TestDecodeManifestRejectsReleaseContractDrift(t *testing.T) {
	valid := validManifestJSON()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "unknown field", raw: strings.Replace(valid, `"version":"0.0.1"`, `"version":"0.0.1","extra":true`, 1)},
		{name: "duplicate nested field", raw: strings.Replace(valid, `"format":"spdx-json"`, `"format":"spdx-json","format":"spdx-json"`, 1)},
		{name: "wrong bundle name", raw: strings.Replace(valid, BundleAssetName, "bundle.zip", 1)},
		{name: "mutable worker image", raw: strings.Replace(valid, "@sha256:", ":latest#", 1)},
		{name: "uppercase source", raw: strings.Replace(valid, strings.Repeat("a", 40), strings.Repeat("A", 40), 1)},
		{name: "zero run ID", raw: strings.Replace(valid, `"qualification_run_id":42`, `"qualification_run_id":0`, 1)},
		{name: "zero run attempt", raw: strings.Replace(valid, `"qualification_run_attempt":3`, `"qualification_run_attempt":0`, 1)},
		{name: "weaker scan", raw: strings.Replace(valid, `"only_fixed":true`, `"only_fixed":false`, 1)},
		{name: "missing tool version", raw: strings.Replace(valid, `"version":"0.148.0"`, `"version":""`, 1)},
		{name: "retired build identity", raw: strings.Replace(valid, `"tools":`, `"build_input_identity":"`+strings.Repeat("d", 64)+`","tools":`, 1)},
		{name: "retired release provenance", raw: strings.Replace(valid, `"repository":"skyhuang233/no-mistakes"`, `"upstream_repository":"kunchenguid/no-mistakes","repository":"skyhuang233/no-mistakes"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeManifest([]byte(test.raw)); err == nil {
				t.Fatalf("DecodeManifest accepted %s", test.name)
			}
		})
	}
}

func TestNormalizeSHA256AcceptsGitHubAssetMetadata(t *testing.T) {
	want := strings.Repeat("a", 64)
	for _, input := range []string{want, "sha256:" + want} {
		got, err := NormalizeSHA256(input)
		if err != nil {
			t.Fatalf("NormalizeSHA256(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeSHA256(%q) = %q", input, got)
		}
	}
	if _, err := NormalizeSHA256("sha256:" + strings.Repeat("A", 64)); err == nil {
		t.Fatal("NormalizeSHA256 accepted uppercase digest")
	}
}
