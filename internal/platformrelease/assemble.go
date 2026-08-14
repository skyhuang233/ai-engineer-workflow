package platformrelease

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

type AssembleOptions struct {
	OutputDirectory    string
	WorkflowExecutable string
	PayloadDirectory   string
	Manifest           Manifest
	SigningKey         *ecdsa.PrivateKey
}

type Assembly struct {
	Directory string
	Manifest  Manifest
}

type packageFile struct {
	Path string
	Data []byte
}

// Assemble builds a byte-reproducible Windows amd64 package and its canonical
// release metadata. The detached ECDSA signature is intentionally not expected
// to be byte-reproducible, but always verifies the same canonical manifest.
func Assemble(options AssembleOptions) (Assembly, error) {
	if options.SigningKey == nil {
		return Assembly{}, errors.New("Platform Release signing key is required")
	}
	files, err := collectPackageFiles(options.WorkflowExecutable, options.PayloadDirectory)
	if err != nil {
		return Assembly{}, err
	}
	archive, err := buildDeterministicZip(files)
	if err != nil {
		return Assembly{}, err
	}
	bundled := make([]BundledFile, 0, len(files))
	for _, file := range files {
		sum := sha256.Sum256(file.Data)
		bundled = append(bundled, BundledFile{Path: file.Path, SHA256: hex.EncodeToString(sum[:])})
	}
	sbom, err := buildSBOM(options.Manifest.Release.Version, bundled)
	if err != nil {
		return Assembly{}, err
	}
	provenance, err := buildProvenance(options.Manifest, archive, sbom)
	if err != nil {
		return Assembly{}, err
	}
	artifactData := map[string][]byte{
		"workflow-windows-amd64.zip": archive,
		"platform-sbom.spdx.json":    sbom,
		"platform-provenance.json":   provenance,
	}
	artifacts := artifactsFor(artifactData)
	manifest := options.Manifest
	manifest.Artifacts = artifacts
	manifest.BundledFiles = bundled
	manifest.Provenance.Subjects = append([]Artifact(nil), artifacts...)
	if err := manifest.Validate(); err != nil {
		return Assembly{}, fmt.Errorf("validate assembled Platform Release Manifest: %w", err)
	}
	manifestRaw, _, err := manifest.Canonical()
	if err != nil {
		return Assembly{}, err
	}
	signature, err := Sign(manifestRaw, options.SigningKey)
	if err != nil {
		return Assembly{}, err
	}
	allFiles := map[string][]byte{
		"workflow-windows-amd64.zip": archive,
		"platform-sbom.spdx.json":    sbom,
		"platform-provenance.json":   provenance,
		"platform-release.json":      manifestRaw,
		"platform-release.json.sig":  signature,
	}
	allFiles["SHA256SUMS"] = checksumFile(allFiles)
	if err := os.MkdirAll(options.OutputDirectory, 0o755); err != nil {
		return Assembly{}, fmt.Errorf("create Platform Release output: %w", err)
	}
	for name, data := range allFiles {
		if err := os.WriteFile(filepath.Join(options.OutputDirectory, name), data, 0o644); err != nil {
			return Assembly{}, fmt.Errorf("write Platform Release artifact %q: %w", name, err)
		}
	}
	return Assembly{Directory: options.OutputDirectory, Manifest: manifest}, nil
}

func collectPackageFiles(workflowExecutable, payloadDirectory string) ([]packageFile, error) {
	executable, err := os.ReadFile(workflowExecutable)
	if err != nil {
		return nil, fmt.Errorf("read workflow executable: %w", err)
	}
	files := []packageFile{{Path: "bin/workflow.exe", Data: executable}}
	err = filepath.WalkDir(payloadDirectory, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("package input %q must be a regular file", filePath)
		}
		relative, err := filepath.Rel(payloadDirectory, filePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "bin/workflow.exe" || strings.HasPrefix(relative, "../") {
			return fmt.Errorf("package input path %q conflicts with a reserved path", relative)
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		files = append(files, packageFile{Path: relative, Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collect Platform Release payload: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func buildDeterministicZip(files []packageFile) ([]byte, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	fixedTime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.Path, Method: zip.Deflate}
		header.SetModTime(fixedTime)
		header.SetMode(0o644)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("create archive entry %q: %w", file.Path, err)
		}
		if _, err := writer.Write(file.Data); err != nil {
			return nil, fmt.Errorf("write archive entry %q: %w", file.Path, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close Platform Release archive: %w", err)
	}
	return buffer.Bytes(), nil
}

func buildSBOM(version string, files []BundledFile) ([]byte, error) {
	document := struct {
		SPDXVersion string        `json:"spdxVersion"`
		DataLicense string        `json:"dataLicense"`
		SPDXID      string        `json:"SPDXID"`
		Name        string        `json:"name"`
		Version     string        `json:"version"`
		Files       []BundledFile `json:"files"`
	}{"SPDX-2.3", "CC0-1.0", "SPDXRef-DOCUMENT", "workflow-platform", version, files}
	return canonicalJSON(document)
}

func buildProvenance(manifest Manifest, archive, sbom []byte) ([]byte, error) {
	type subject struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	statement := struct {
		PredicateType string    `json:"predicate_type"`
		Repository    string    `json:"repository"`
		SourceCommit  string    `json:"source_commit"`
		WorkflowPath  string    `json:"workflow_path"`
		RunID         int64     `json:"github_actions_run_id"`
		Subjects      []subject `json:"subjects"`
	}{
		PredicateType: "https://slsa.dev/provenance/v1",
		Repository:    manifest.Provenance.Repository, SourceCommit: manifest.Provenance.SourceCommit,
		WorkflowPath: manifest.Provenance.WorkflowPath, RunID: manifest.Provenance.GitHubActionsRunID,
		Subjects: []subject{{Name: "platform-sbom.spdx.json", SHA256: digestBytes(sbom)}, {Name: "workflow-windows-amd64.zip", SHA256: digestBytes(archive)}},
	}
	return canonicalJSON(statement)
}

func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, _, err := setupcontract.Canonicalize(raw)
	return canonical, err
}

func artifactsFor(data map[string][]byte) []Artifact {
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	artifacts := make([]Artifact, 0, len(names))
	for _, name := range names {
		artifacts = append(artifacts, Artifact{Name: name, SHA256: digestBytes(data[name]), Size: int64(len(data[name]))})
	}
	return artifacts
}

func checksumFile(files map[string][]byte, excluded ...string) []byte {
	exclude := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		exclude[name] = struct{}{}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name == "SHA256SUMS" {
			continue
		}
		if _, skip := exclude[name]; skip {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		fmt.Fprintf(&output, "%s  %s\n", digestBytes(files[name]), name)
	}
	return []byte(output.String())
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
