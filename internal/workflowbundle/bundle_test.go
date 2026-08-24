package workflowbundle

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAssembleBundleCreatesExactlyOneSelfContainedAsset(t *testing.T) {
	root := t.TempDir()
	setup, workflow := filepath.Join(root, "workflow-setup.exe"), filepath.Join(root, "workflow.exe")
	if err := os.WriteFile(setup, []byte("setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflow, []byte("workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "payload")
	for name, data := range map[string]string{"skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"} {
		path := filepath.Join(payload, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(root, "release", "workflow-windows-amd64.zip")
	manifest := BundleManifest{SchemaVersion: 1, SetupProtocolVersion: 1, Version: "0.0.1", Compatibility: Compatibility{OS: "windows", Architecture: "amd64", DatabaseSchema: 63, DockerDesktopVersion: "4.86.0", DockerInstallerURL: "https://example.test/docker.exe", DockerInstallerSHA256: strings.Repeat("b", 64), WorkerImage: "ghcr.io/skyhuang233/workflow-worker@sha256:" + strings.Repeat("a", 64)}}
	if err := AssembleBundle(BundleAssembleOptions{Output: output, SetupExecutable: setup, WorkflowExecutable: workflow, PayloadDirectory: payload, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	names := []string{}
	for _, entry := range archive.File {
		names = append(names, entry.Name)
	}
	want := []string{"platform-release.json", "platform/workflow.exe", "repository-contract/repository.json", "setup/workflow-setup.exe", "skills/agent-workflow/SKILL.md"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entries=%v want %v", names, want)
	}
}
