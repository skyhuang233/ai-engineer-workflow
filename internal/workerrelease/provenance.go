package workerrelease

import (
	"errors"
	"strings"

	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

type ToolProvenance struct {
	Version           string
	SourceCommit      string
	ImageReference    string
	CodexVersion      string
	GitHubCLIVersion  string
	GoVersion         string
	NoMistakesVersion string
}

func DecodeToolProvenance(raw []byte) (ToolProvenance, error) {
	manifest, err := workflowrelease.DecodeManifest(raw)
	if err != nil {
		return ToolProvenance{}, err
	}
	provenance := ToolProvenance{
		Version: manifest.Version, SourceCommit: manifest.SourceCommit,
		ImageReference:    manifest.Worker.Image,
		CodexVersion:      manifest.Worker.Tools.Codex.Version,
		GitHubCLIVersion:  manifest.Worker.Tools.GitHubCLI.Version,
		GoVersion:         manifest.Worker.Tools.Go.Version,
		NoMistakesVersion: manifest.Worker.Tools.NoMistakes.Version,
	}
	if _, err := provenance.ToolVersions(); err != nil {
		return ToolProvenance{}, err
	}
	return provenance, nil
}

func (p ToolProvenance) ToolVersions() (map[string]string, error) {
	switch {
	case strings.TrimSpace(p.CodexVersion) == "":
		return nil, errors.New("Workflow Release Manifest Codex version is required")
	case strings.TrimSpace(p.GitHubCLIVersion) == "":
		return nil, errors.New("Workflow Release Manifest GitHub CLI version is required")
	case strings.TrimSpace(p.GoVersion) == "":
		return nil, errors.New("Workflow Release Manifest Go version is required")
	case strings.TrimSpace(p.NoMistakesVersion) == "":
		return nil, errors.New("Workflow Release Manifest no-mistakes version is required")
	}
	return map[string]string{
		"codex":       p.CodexVersion,
		"github-cli":  p.GitHubCLIVersion,
		"go":          p.GoVersion,
		"no-mistakes": p.NoMistakesVersion,
	}, nil
}
