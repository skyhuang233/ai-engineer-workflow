// Package platformrelease defines the complete, signed contract that permits a
// bootstrap skill to plan an installation without trusting an installed CLI.
package platformrelease

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

const ManifestSchemaVersion = 1

type Manifest struct {
	SchemaVersion     int                   `json:"schema_version"`
	Release           ReleaseMetadata       `json:"release"`
	BootstrapContract SchemaRange           `json:"bootstrap_contract"`
	PlatformSetup     PlatformSetupContract `json:"platform_setup_contract"`
	Artifacts         []Artifact            `json:"artifacts"`
	BundledFiles      []BundledFile         `json:"bundled_files"`
	Provenance        Provenance            `json:"provenance"`
	Signature         SignatureMetadata     `json:"signature"`
}

type ReleaseMetadata struct {
	Version            string `json:"version"`
	Channel            string `json:"channel"`
	Repository         string `json:"repository"`
	Tag                string `json:"tag"`
	SourceCommit       string `json:"source_commit"`
	GitHubActionsRunID int64  `json:"github_actions_run_id"`
}

type SchemaRange struct {
	MinimumSchema int `json:"minimum_schema"`
	MaximumSchema int `json:"maximum_schema"`
}

type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type BundledFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Provenance struct {
	Repository         string     `json:"repository"`
	SourceCommit       string     `json:"source_commit"`
	WorkflowPath       string     `json:"workflow_path"`
	GitHubActionsRunID int64      `json:"github_actions_run_id"`
	BuilderID          string     `json:"builder_id"`
	Subjects           []Artifact `json:"subjects"`
}

type SignatureMetadata struct {
	Algorithm      string `json:"algorithm"`
	KeyID          string `json:"key_id"`
	SignatureAsset string `json:"signature_asset"`
}

func (m Manifest) Canonical() ([]byte, string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, "", fmt.Errorf("encode Platform Release Manifest: %w", err)
	}
	canonical, digest, err := setupcontract.Canonicalize(raw)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize Platform Release Manifest: %w", err)
	}
	return canonical, digest, nil
}

func Parse(raw []byte) (Manifest, []byte, string, error) {
	canonical, digest, err := setupcontract.Canonicalize(raw)
	if err != nil {
		return Manifest{}, nil, "", err
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, nil, "", fmt.Errorf("decode Platform Release Manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Manifest{}, nil, "", fmt.Errorf("Platform Release Manifest has trailing JSON")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, nil, "", err
	}
	return manifest, canonical, digest, nil
}
