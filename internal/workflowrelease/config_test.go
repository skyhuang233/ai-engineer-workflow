package workflowrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAcceptsTheUnpublishedWorkflowBaseline(t *testing.T) {
	config, err := LoadConfig(filepath.Join("..", "..", "config", "workflow-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Version != "0.0.0" {
		t.Fatalf("version = %q, want unpublished baseline 0.0.0", config.Version)
	}
	if config.DockerDesktop.Version != "4.86.0" {
		t.Fatalf("Docker Desktop version = %q", config.DockerDesktop.Version)
	}
}

func TestDecodeConfigRejectsContractDrift(t *testing.T) {
	valid := `{"schema_version":1,"version":"0.0.0","docker_desktop":{"version":"4.86.0","installer_url":"https://example.test/docker.exe","windows_amd64_sha256":"` + strings.Repeat("a", 64) + `"}}`
	tests := []struct {
		name string
		raw  string
	}{
		{name: "unknown field", raw: strings.Replace(valid, `"version":"0.0.0"`, `"version":"0.0.0","extra":true`, 1)},
		{name: "duplicate field", raw: strings.Replace(valid, `"version":"0.0.0"`, `"version":"0.0.0","version":"0.0.1"`, 1)},
		{name: "non HTTPS installer", raw: strings.Replace(valid, "https://example.test", "http://example.test", 1)},
		{name: "uppercase digest", raw: strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)},
		{name: "prerelease version", raw: strings.Replace(valid, "0.0.0", "0.0.1-rc.1", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeConfig([]byte(test.raw)); err == nil {
				t.Fatalf("DecodeConfig accepted %s", test.name)
			}
		})
	}
}

func TestLoadConfigRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow-release.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"version":"0.0.0","docker_desktop":{"version":"4.86.0","installer_url":"https://example.test/docker.exe","windows_amd64_sha256":"`+strings.Repeat("a", 64)+`"}} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted trailing JSON")
	}
}
