package workerrelease

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ToolProvenance struct {
	CodexVersion      string `json:"codex_version"`
	GitHubCLIVersion  string `json:"github_cli_version"`
	GoVersion         string `json:"go_version"`
	NoMistakesVersion string `json:"no_mistakes_version"`
}

func DecodeToolProvenance(raw []byte) (ToolProvenance, error) {
	var provenance ToolProvenance
	if err := json.Unmarshal(raw, &provenance); err != nil {
		return ToolProvenance{}, fmt.Errorf("decode Worker Release tool provenance: %w", err)
	}
	if _, err := provenance.ToolVersions(); err != nil {
		return ToolProvenance{}, err
	}
	return provenance, nil
}

func (p ToolProvenance) ToolVersions() (map[string]string, error) {
	switch {
	case strings.TrimSpace(p.CodexVersion) == "":
		return nil, errors.New("Worker Release Manifest Codex version is required")
	case strings.TrimSpace(p.GitHubCLIVersion) == "":
		return nil, errors.New("Worker Release Manifest GitHub CLI version is required")
	case strings.TrimSpace(p.GoVersion) == "":
		return nil, errors.New("Worker Release Manifest Go version is required")
	case strings.TrimSpace(p.NoMistakesVersion) == "":
		return nil, errors.New("Worker Release Manifest no-mistakes version is required")
	}
	return map[string]string{
		"codex":       p.CodexVersion,
		"github-cli":  p.GitHubCLIVersion,
		"go":          p.GoVersion,
		"no-mistakes": p.NoMistakesVersion,
	}, nil
}
