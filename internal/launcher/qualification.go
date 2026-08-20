package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

const (
	setupQualificationEnvironment    = "WORKFLOW_SETUP_QUALIFICATION"
	candidateDirectoryEnvironment    = "WORKFLOW_SETUP_CANDIDATE_DIRECTORY"
	candidateVersionEnvironment      = "WORKFLOW_SETUP_CANDIDATE_VERSION"
	candidateSourceCommitEnvironment = "WORKFLOW_SETUP_CANDIDATE_SOURCE_COMMIT"
)

func validateQualificationCandidateRequest(request Request) error {
	enabled := os.Getenv(setupQualificationEnvironment) == "1"
	if request.QualificationCandidate == nil {
		if enabled {
			return errors.New("qualification setup requires an authenticated candidate manifest")
		}
		return nil
	}
	if !enabled {
		return errors.New("qualification candidate is disabled")
	}
	candidate := request.QualificationCandidate
	directory := os.Getenv(candidateDirectoryEnvironment)
	if directory == "" || !filepath.IsAbs(directory) || !filepath.IsAbs(candidate.ManifestPath) {
		return errors.New("qualification candidate paths must be absolute")
	}
	expectedPath := filepath.Join(filepath.Clean(directory), workflowrelease.ManifestAssetName)
	if !strings.EqualFold(filepath.Clean(candidate.ManifestPath), expectedPath) {
		return errors.New("qualification candidate manifest path differs from the configured directory")
	}
	if os.Getenv(candidateVersionEnvironment) != request.TargetVersion {
		return errors.New("qualification candidate version differs from the setup target")
	}
	expectedSource := os.Getenv(candidateSourceCommitEnvironment)
	if expectedSource == "" || candidate.SourceCommit != expectedSource || len(expectedSource) != 40 || strings.ToLower(expectedSource) != expectedSource {
		return errors.New("qualification candidate source commit differs from the configured source")
	}
	if _, err := hex.DecodeString(expectedSource); err != nil {
		return errors.New("qualification candidate source commit differs from the configured source")
	}
	if len(candidate.ManifestSHA256) != 64 || strings.ToLower(candidate.ManifestSHA256) != candidate.ManifestSHA256 {
		return errors.New("qualification candidate manifest SHA-256 is invalid")
	}
	if _, err := hex.DecodeString(candidate.ManifestSHA256); err != nil {
		return errors.New("qualification candidate manifest SHA-256 is invalid")
	}
	return nil
}

func (e Engine) qualificationWorkerRelease(request Request) (*store.WorkerRelease, error) {
	if err := validateQualificationCandidateRequest(request); err != nil {
		return nil, err
	}
	if request.QualificationCandidate == nil {
		return nil, nil
	}
	candidate := request.QualificationCandidate
	raw, err := os.ReadFile(candidate.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read qualification candidate manifest: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != candidate.ManifestSHA256 {
		return nil, errors.New("qualification candidate manifest digest changed after authentication")
	}
	manifest, err := workflowrelease.DecodeManifest(raw)
	if err != nil {
		return nil, err
	}
	if manifest.Version != request.TargetVersion || manifest.SourceCommit != candidate.SourceCommit || manifest.Bundle.SHA256 != strings.TrimPrefix(request.BundleDigest, "sha256:") {
		return nil, errors.New("qualification candidate manifest differs from the exact setup target")
	}
	compatibility, err := e.bundleCompatibility()
	if err != nil {
		return nil, err
	}
	if manifest.Worker.Image != compatibility.WorkerImage {
		return nil, errors.New("qualification candidate Worker image differs from the verified Bundle")
	}
	return &store.WorkerRelease{Version: manifest.Version, SourceCommit: manifest.SourceCommit, ImageReference: manifest.Worker.Image, ManifestJSON: string(raw)}, nil
}
