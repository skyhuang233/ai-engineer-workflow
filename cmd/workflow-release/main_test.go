package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

func TestAssembleProducesOneAtomicWorkflowRelease(t *testing.T) {
	repositoryRoot := filepath.Join("..", "..")
	root := t.TempDir()
	configPath := filepath.Join(repositoryRoot, "config", "workflow-release.json")
	config, err := workflowrelease.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	workflowExecutable := filepath.Join(root, "workflow.exe")
	versionProbeSource := filepath.Join(root, "version-probe.go")
	versionProbe := fmt.Sprintf("package main\nimport \"fmt\"\nfunc main(){fmt.Println(%q)}\n", "workflow "+config.Version)
	if err := os.WriteFile(versionProbeSource, []byte(versionProbe), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", workflowExecutable, versionProbeSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Workflow executable: %v\n%s", err, output)
	}
	setupExecutable := filepath.Join(root, "workflow-setup.exe")
	if err := os.WriteFile(setupExecutable, []byte("setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "payload")
	for name, body := range map[string]string{"skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"} {
		path := filepath.Join(payload, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sbom := filepath.Join(root, "generated.spdx.json")
	if err := os.WriteFile(sbom, []byte(`{"spdxVersion":"SPDX-2.3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDirectory := filepath.Join(root, "release")
	image := workflowrelease.WorkerRepository + "@sha256:" + strings.Repeat("c", 64)
	if err := run([]string{
		"assemble",
		"-config", configPath,
		"-toolchain", filepath.Join(repositoryRoot, "config", "toolchain.json"),
		"-workflow-exe", workflowExecutable,
		"-setup-exe", setupExecutable,
		"-payload", payload,
		"-output", outputDirectory,
		"-source-commit", strings.Repeat("a", 40),
		"-github-actions-run-id", "42",
		"-worker-image", image,
		"-sbom", sbom,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outputDirectory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	wantNames := []string{workflowrelease.SBOMAssetName, workflowrelease.ManifestAssetName, workflowrelease.BundleAssetName}
	sort.Strings(wantNames)
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("release assets = %v, want %v", names, wantNames)
	}
	rawManifest, err := os.ReadFile(filepath.Join(outputDirectory, workflowrelease.ManifestAssetName))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := workflowrelease.DecodeManifest(rawManifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Worker.Image != image || manifest.Version != config.Version || manifest.Worker.Tools.NoMistakes.Commit == "" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestAssembleRejectsMissingVerifiedInputs(t *testing.T) {
	if err := run([]string{"assemble"}); err == nil {
		t.Fatal("assemble accepted missing verified inputs")
	}
}
