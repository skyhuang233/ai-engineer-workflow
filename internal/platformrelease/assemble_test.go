package platformrelease

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAssembleProducesReproducibleContentAddressedRelease(t *testing.T) {
	inputs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(inputs, "skills", "implement"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputs, "skills", "implement", "SKILL.md"), []byte("# Implement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(inputs, "repository-contract"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputs, "repository-contract", "AGENTS.block.md"), []byte("workflow block\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workflowEXE := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(workflowEXE, []byte("windows-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := validManifest(fixtureArtifacts())
	template.Artifacts = nil
	template.BundledFiles = nil
	template.Provenance.Subjects = nil

	first, err := Assemble(AssembleOptions{OutputDirectory: filepath.Join(t.TempDir(), "first"), WorkflowExecutable: workflowEXE, PayloadDirectory: inputs, Manifest: template, SigningKey: key})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Assemble(AssembleOptions{OutputDirectory: filepath.Join(t.TempDir(), "second"), WorkflowExecutable: workflowEXE, PayloadDirectory: inputs, Manifest: template, SigningKey: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workflow-windows-amd64.zip", "platform-sbom.spdx.json", "platform-provenance.json", "platform-release.json"} {
		left, err := os.ReadFile(filepath.Join(first.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(second.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s is not reproducible", name)
		}
	}
	if !reflect.DeepEqual(first.Manifest.Artifacts, second.Manifest.Artifacts) || !reflect.DeepEqual(first.Manifest.BundledFiles, second.Manifest.BundledFiles) {
		t.Fatal("assembled release digests changed with identical inputs")
	}
	archive, err := zip.OpenReader(filepath.Join(first.Directory, "workflow-windows-amd64.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	var names []string
	for _, file := range archive.File {
		names = append(names, file.Name)
		if file.Modified.Year() != 1980 {
			t.Fatalf("archive timestamp for %s is not normalized: %s", file.Name, file.Modified)
		}
	}
	wantNames := []string{"bin/workflow.exe", "repository-contract/AGENTS.block.md", "skills/implement/SKILL.md"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("archive entries = %v, want %v", names, wantNames)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(first.Directory, "platform-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	signature, err := os.ReadFile(filepath.Join(first.Directory, "platform-release.json.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedManifest(manifestRaw, signature, &key.PublicKey, TrustPolicy{Repository: template.Release.Repository, WorkflowPath: template.Provenance.WorkflowPath, KeyID: template.Signature.KeyID}); err != nil {
		t.Fatal(err)
	}
}
