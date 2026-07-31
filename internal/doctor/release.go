package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type WorkerReleaseManifest struct {
	SchemaVersion      int    `json:"schema_version"`
	WorkerVersion      string `json:"worker_version"`
	SourceCommit       string `json:"source_commit"`
	Image              string `json:"image"`
	CodexVersion       string `json:"codex_version"`
	NoMistakesVersion  string `json:"no_mistakes_version"`
	NoMistakesCommit   string `json:"no_mistakes_commit"`
	GitHubActionsRunID int64  `json:"github_actions_run_id"`
}

func LoadWorkerReleaseManifest(path string, config Config) (WorkerReleaseManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkerReleaseManifest{}, err
	}
	var manifest WorkerReleaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return WorkerReleaseManifest{}, fmt.Errorf("decode Worker Release Manifest: %w", err)
	}
	if err := manifest.Validate(config); err != nil {
		return WorkerReleaseManifest{}, err
	}
	return manifest, nil
}

func (m WorkerReleaseManifest) Validate(config Config) error {
	switch {
	case m.SchemaVersion != 1:
		return errors.New("unsupported Worker Release Manifest schema")
	case m.WorkerVersion != config.Worker.Version:
		return errors.New("Worker Release version does not match toolchain")
	case !shaPattern.MatchString(m.SourceCommit):
		return errors.New("Worker Release source commit must be a full SHA")
	case !imagePattern.MatchString(m.Image) || !strings.HasPrefix(m.Image, config.Worker.ImageRepository+"@"):
		return errors.New("Worker Release image does not match the immutable toolchain repository")
	case m.CodexVersion != config.Codex.Version:
		return errors.New("Worker Release Codex version does not match toolchain")
	case m.NoMistakesVersion != config.NoMistakes.Version || m.NoMistakesCommit != config.NoMistakes.UpstreamCommit:
		return errors.New("Worker Release no-mistakes pin does not match toolchain")
	case m.GitHubActionsRunID <= 0:
		return errors.New("Worker Release Actions run ID is required")
	default:
		return nil
	}
}
