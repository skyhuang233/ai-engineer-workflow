package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlatformPublisherBuildInjectsTheExactManifestAndTagVersion(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "publish-platform.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(body)
	for _, required := range []string{
		`PLATFORM_VERSION: ${{ vars.WORKFLOW_PLATFORM_VERSION }}`,
		`$env:PLATFORM_VERSION -cnotmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'`,
		`-X main.Version=$env:PLATFORM_VERSION`,
		`-version $env:PLATFORM_VERSION`,
		`tag="platform-v${PLATFORM_VERSION}"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Platform publisher does not bind the Workflow CLI, manifest, and tag through %q", required)
		}
	}
}

func TestPlatformPublisherRejectsNonCanonicalPlatformVersionInput(t *testing.T) {
	for _, version := range []string{"v1.2.3", "01.2.3", "1.2.3-rc.1", "1.2.3+build.1"} {
		t.Run(version, func(t *testing.T) {
			err := run([]string{
				"-workflow-exe", "missing-workflow.exe",
				"-payload", "missing-payload",
				"-output", "missing-output",
				"-version", version,
				"-source-commit", strings.Repeat("a", 40),
				"-github-actions-run-id", "1",
				"-docker-version", "1",
				"-docker-installer-url", "https://example.invalid/docker.exe",
				"-docker-installer-sha256", strings.Repeat("b", 64),
				"-worker-image", "ghcr.io/owner/worker@sha256:" + strings.Repeat("c", 64),
				"-signing-key", "missing-key.pem",
			})
			if err == nil || !strings.Contains(err.Error(), "bare semantic version core") {
				t.Fatalf("publisher accepted version %q: %v", version, err)
			}
		})
	}
}

func TestPlatformPublisherRejectsWorkflowExecutableVersionDifferentFromManifest(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "workflow.exe")
	build := exec.Command("go", "build", "-ldflags", "-X main.Version=1.2.3", "-o", executable, "../workflow")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build versioned workflow executable: %v\n%s", err, output)
	}
	if err := verifyWorkflowExecutableVersion(executable, "1.2.3"); err != nil {
		t.Fatalf("matching executable version: %v", err)
	}
	if err := verifyWorkflowExecutableVersion(executable, "1.2.4"); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("mismatched executable version was publishable: %v", err)
	}
}

func TestTrustKeyCommandEmitsOnlyPublicMetadata(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(repository, "trust"), 0o755); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(t.TempDir(), "offline", "private.pem")
	publicPath := filepath.Join(repository, "trust", "public.pem")
	var output bytes.Buffer
	if err := runTrustKey([]string{"--repository-root", repository, "--private-key", privatePath, "--public-key", publicPath, "--generate"}, &output); err != nil {
		t.Fatal(err)
	}
	privateRaw, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), privateRaw) || strings.Contains(output.String(), "PRIVATE KEY") || strings.Contains(output.String(), privatePath) {
		t.Fatalf("trust-key output exposed private material or location: %s", output.String())
	}
	var result struct {
		PublicKeyPath   string `json:"public_key_path"`
		PublicKeySHA256 string `json:"public_key_sha256"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.PublicKeyPath != publicPath || len(result.PublicKeySHA256) != 64 {
		t.Fatalf("trust-key public result = %#v", result)
	}
}

func TestTrustKeyCommandFailsWithoutExplicitModeInputs(t *testing.T) {
	if err := runTrustKey(nil, &bytes.Buffer{}); err == nil {
		t.Fatal("trust-key accepted implicit paths or generation")
	}
}
