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

func validateVerifiedReleaseManifestRequest(request Request, required bool) error {
	enabled := os.Getenv(setupQualificationEnvironment) == "1"
	if request.VerifiedReleaseManifest == nil {
		if enabled || required {
			return errors.New("setup target requires a verified release manifest")
		}
		return nil
	}
	verified := request.VerifiedReleaseManifest
	if !filepath.IsAbs(verified.ManifestPath) {
		return errors.New("verified release manifest path must be absolute")
	}
	if len(verified.SourceCommit) != 40 || strings.ToLower(verified.SourceCommit) != verified.SourceCommit {
		return errors.New("verified release manifest source commit is invalid")
	}
	if _, err := hex.DecodeString(verified.SourceCommit); err != nil {
		return errors.New("verified release manifest source commit is invalid")
	}
	if len(verified.ManifestSHA256) != 64 || strings.ToLower(verified.ManifestSHA256) != verified.ManifestSHA256 {
		return errors.New("verified release manifest SHA-256 is invalid")
	}
	if _, err := hex.DecodeString(verified.ManifestSHA256); err != nil {
		return errors.New("verified release manifest SHA-256 is invalid")
	}
	if !enabled {
		return nil
	}
	directory := os.Getenv(candidateDirectoryEnvironment)
	if directory == "" || !filepath.IsAbs(directory) {
		return errors.New("qualification candidate directory must be absolute")
	}
	expectedPath := filepath.Join(filepath.Clean(directory), workflowrelease.ManifestAssetName)
	if !strings.EqualFold(filepath.Clean(verified.ManifestPath), expectedPath) {
		return errors.New("qualification candidate manifest path differs from the configured directory")
	}
	if os.Getenv(candidateVersionEnvironment) != request.TargetVersion {
		return errors.New("qualification candidate version differs from the setup target")
	}
	if expectedSource := os.Getenv(candidateSourceCommitEnvironment); expectedSource == "" || verified.SourceCommit != expectedSource {
		return errors.New("qualification candidate source commit differs from the configured source")
	}
	return nil
}

func (e Engine) verifiedWorkerRelease(request Request) (*store.WorkerRelease, error) {
	if err := validateVerifiedReleaseManifestRequest(request, false); err != nil {
		return nil, err
	}
	if request.VerifiedReleaseManifest == nil {
		return nil, nil
	}
	verified := request.VerifiedReleaseManifest
	raw, err := os.ReadFile(verified.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read verified release manifest: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != verified.ManifestSHA256 {
		return nil, errors.New("verified release manifest digest changed after authentication")
	}
	manifest, err := workflowrelease.DecodeManifest(raw)
	if err != nil {
		return nil, err
	}
	if manifest.Version != request.TargetVersion || manifest.SourceCommit != verified.SourceCommit || manifest.Bundle.SHA256 != strings.TrimPrefix(request.BundleDigest, "sha256:") {
		return nil, errors.New("verified release manifest differs from the exact setup target")
	}
	compatibility, err := e.bundleCompatibility()
	if err != nil {
		return nil, err
	}
	if manifest.Worker.Image != compatibility.WorkerImage {
		return nil, errors.New("verified release manifest Worker image differs from the verified Bundle")
	}
	return &store.WorkerRelease{Version: manifest.Version, SourceCommit: manifest.SourceCommit, ImageReference: manifest.Worker.Image, ManifestJSON: string(raw)}, nil
}
