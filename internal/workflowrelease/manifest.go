package workflowrelease

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	ManifestAssetName = "workflow-release.json"
	BundleAssetName   = "workflow-windows-amd64.zip"
	SBOMAssetName     = "worker-sbom.spdx.json"
	WorkerRepository  = "ghcr.io/skyhuang233/workflow-worker"
)

var (
	hex40Pattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type Manifest struct {
	SchemaVersion         int    `json:"schema_version"`
	Version               string `json:"version"`
	CandidateSourceCommit string `json:"candidate_source_commit"`
	QualificationRunID    int64  `json:"qualification_run_id"`
	Bundle                Bundle `json:"bundle"`
	Worker                Worker `json:"worker"`
	SBOM                  SBOM   `json:"sbom"`
}

type Bundle struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type Worker struct {
	Image string `json:"image"`
	Tools Tools  `json:"tools"`
}

type Tools struct {
	Codex      CodexTool      `json:"codex"`
	GitHubCLI  ArchiveTool    `json:"github_cli"`
	Go         ArchiveTool    `json:"go"`
	NoMistakes NoMistakesTool `json:"no_mistakes"`
}

type CodexTool struct {
	Version string `json:"version"`
}

type ArchiveTool struct {
	Version          string `json:"version"`
	LinuxAMD64SHA256 string `json:"linux_amd64_sha256"`
}

type NoMistakesTool struct {
	Version    string `json:"version"`
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

type SBOM struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	SHA256 string `json:"sha256"`
	Scan   Scan   `json:"scan"`
}

type Scan struct {
	Scanner        string `json:"scanner"`
	SeverityCutoff string `json:"severity_cutoff"`
	OnlyFixed      bool   `json:"only_fixed"`
}

func DecodeManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeStrict(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode Workflow Release Manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	switch {
	case m.SchemaVersion != 1:
		return errors.New("unsupported Workflow Release Manifest schema")
	case validateSemver(m.Version) != nil:
		return fmt.Errorf("version: %w", validateSemver(m.Version))
	case !hex40Pattern.MatchString(m.CandidateSourceCommit):
		return errors.New("source commit must be a lowercase 40-character Git object ID")
	case m.QualificationRunID <= 0:
		return errors.New("GitHub Actions run ID must be positive")
	case m.Bundle.Name != BundleAssetName:
		return fmt.Errorf("Bundle name must be %s", BundleAssetName)
	case !hex64Pattern.MatchString(m.Bundle.SHA256):
		return errors.New("Bundle SHA-256 must be lowercase hexadecimal")
	case m.Worker.Image != WorkerRepository+"@sha256:"+strings.TrimPrefix(m.Worker.Image, WorkerRepository+"@sha256:") ||
		!strings.HasPrefix(m.Worker.Image, WorkerRepository+"@sha256:") ||
		!hex64Pattern.MatchString(strings.TrimPrefix(m.Worker.Image, WorkerRepository+"@sha256:")):
		return fmt.Errorf("Worker image must be an immutable %s digest reference", WorkerRepository)
	case m.SBOM.Name != SBOMAssetName:
		return fmt.Errorf("SBOM name must be %s", SBOMAssetName)
	case m.SBOM.Format != "spdx-json":
		return errors.New("SBOM format must be spdx-json")
	case !hex64Pattern.MatchString(m.SBOM.SHA256):
		return errors.New("SBOM SHA-256 must be lowercase hexadecimal")
	case m.SBOM.Scan.Scanner != "grype" || m.SBOM.Scan.SeverityCutoff != "high" || !m.SBOM.Scan.OnlyFixed:
		return errors.New("SBOM scan policy must be Grype high severity with only-fixed enabled")
	}
	return m.Worker.Tools.Validate()
}

func (m Manifest) Canonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func (t Tools) Validate() error {
	switch {
	case strings.TrimSpace(t.Codex.Version) == "":
		return errors.New("Codex version is required")
	case strings.TrimSpace(t.GitHubCLI.Version) == "" || !hex64Pattern.MatchString(t.GitHubCLI.LinuxAMD64SHA256):
		return errors.New("GitHub CLI version and Linux amd64 SHA-256 are required")
	case strings.TrimSpace(t.Go.Version) == "" || !hex64Pattern.MatchString(t.Go.LinuxAMD64SHA256):
		return errors.New("Go version and Linux amd64 SHA-256 are required")
	case strings.TrimSpace(t.NoMistakes.Version) == "":
		return errors.New("no-mistakes version is required")
	case !repositoryPattern.MatchString(t.NoMistakes.Repository) || !hex40Pattern.MatchString(t.NoMistakes.Commit):
		return errors.New("no-mistakes repository and commit are invalid")
	}
	return nil
}

func NormalizeSHA256(value string) (string, error) {
	normalized := strings.TrimPrefix(value, "sha256:")
	if !hex64Pattern.MatchString(normalized) {
		return "", errors.New("SHA-256 must be lowercase hexadecimal with an optional sha256: prefix")
	}
	return normalized, nil
}

type ManifestOptions struct {
	Config                Config
	CandidateSourceCommit string
	QualificationRunID    int64
	BundlePath            string
	WorkerImage           string
	Tools                 Tools
	SBOMPath              string
}

func CreateManifest(options ManifestOptions) (Manifest, error) {
	if err := options.Config.Validate(); err != nil {
		return Manifest{}, err
	}
	bundleDigest, err := fileSHA256(options.BundlePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("hash Workflow Bundle: %w", err)
	}
	sbomDigest, err := fileSHA256(options.SBOMPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("hash Worker SBOM: %w", err)
	}
	manifest := Manifest{
		SchemaVersion:         1,
		Version:               options.Config.Version,
		CandidateSourceCommit: options.CandidateSourceCommit,
		QualificationRunID:    options.QualificationRunID,
		Bundle:                Bundle{Name: BundleAssetName, SHA256: bundleDigest},
		Worker:                Worker{Image: options.WorkerImage, Tools: options.Tools},
		SBOM:                  SBOM{Name: SBOMAssetName, Format: "spdx-json", SHA256: sbomDigest, Scan: Scan{Scanner: "grype", SeverityCutoff: "high", OnlyFixed: true}},
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
