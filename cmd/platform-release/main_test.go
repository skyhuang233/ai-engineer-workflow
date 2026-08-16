package main

import (
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
		`config/platform-release.json`,
		`steps.release.outputs.version`,
		`2147483647`,
		`-X main.Version=$env:PLATFORM_VERSION`,
		`-version '${{ steps.release.outputs.version }}'`,
		`tag="platform-v${PLATFORM_VERSION}"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Platform publisher does not bind the Workflow CLI, manifest, and tag through %q", required)
		}
	}
}

func TestPlatformPublisherRejectsNonCanonicalPlatformVersionInput(t *testing.T) {
	for _, version := range []string{"v1.2.3", "01.2.3", "1.2.3-rc.1", "1.2.3+build.1", "2147483648.0.0"} {
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
			})
			if err == nil || (!strings.Contains(err.Error(), "bare semantic version core") && !strings.Contains(err.Error(), "signed 32-bit range")) {
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

func TestPlatformPublisherExposesNoSigningKeyInterface(t *testing.T) {
	if err := run([]string{"-signing-key", "private.pem"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("publisher retained signing-key input: %v", err)
	}
	if err := run([]string{"trust-key"}); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("publisher retained trust-key command: %v", err)
	}
}
