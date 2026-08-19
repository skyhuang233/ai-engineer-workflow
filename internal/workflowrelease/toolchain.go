package workflowrelease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type ToolchainConfig struct {
	SchemaVersion int             `json:"schema_version"`
	Codex         CodexTool       `json:"codex"`
	GitHubCLI     ArchiveTool     `json:"github_cli"`
	Go            ArchiveTool     `json:"go"`
	NoMistakes    NoMistakesTool  `json:"no_mistakes"`
	Worker        ToolchainWorker `json:"worker"`
	Runtime       json.RawMessage `json:"runtime"`
	GitHub        json.RawMessage `json:"github"`
	Upgrade       json.RawMessage `json:"upgrade"`
}

type ToolchainWorker struct {
	ImageRepository   string `json:"image_repository"`
	ReleaseRepository string `json:"release_repository"`
}

func LoadToolchain(path string) (ToolchainConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ToolchainConfig{}, fmt.Errorf("read toolchain configuration: %w", err)
	}
	return DecodeToolchain(raw)
}

func DecodeToolchain(raw []byte) (ToolchainConfig, error) {
	type envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Codex         json.RawMessage `json:"codex"`
		GitHubCLI     json.RawMessage `json:"github_cli"`
		Go            json.RawMessage `json:"go"`
		NoMistakes    json.RawMessage `json:"no_mistakes"`
		Worker        json.RawMessage `json:"worker"`
		Runtime       json.RawMessage `json:"runtime"`
		GitHub        json.RawMessage `json:"github"`
		Upgrade       json.RawMessage `json:"upgrade"`
	}
	var encoded envelope
	if err := decodeStrict(raw, &encoded); err != nil {
		return ToolchainConfig{}, fmt.Errorf("decode toolchain configuration: %w", err)
	}
	config := ToolchainConfig{
		SchemaVersion: encoded.SchemaVersion,
		Runtime:       encoded.Runtime,
		GitHub:        encoded.GitHub,
		Upgrade:       encoded.Upgrade,
	}
	for name, pair := range map[string]struct {
		raw    json.RawMessage
		target any
	}{
		"codex":       {encoded.Codex, &config.Codex},
		"github_cli":  {encoded.GitHubCLI, &config.GitHubCLI},
		"go":          {encoded.Go, &config.Go},
		"no_mistakes": {encoded.NoMistakes, &config.NoMistakes},
		"worker":      {encoded.Worker, &config.Worker},
	} {
		if err := decodeStrict(pair.raw, pair.target); err != nil {
			return ToolchainConfig{}, fmt.Errorf("decode toolchain configuration %s: %w", name, err)
		}
	}
	if config.SchemaVersion != 7 {
		return ToolchainConfig{}, errors.New("unsupported toolchain configuration schema")
	}
	if err := config.Tools().Validate(); err != nil {
		return ToolchainConfig{}, err
	}
	if config.Worker.ImageRepository != WorkerRepository {
		return ToolchainConfig{}, fmt.Errorf("Worker image repository must be %s", WorkerRepository)
	}
	if !repositoryPattern.MatchString(config.Worker.ReleaseRepository) {
		return ToolchainConfig{}, errors.New("Workflow Release repository is invalid")
	}
	return config, nil
}

func (c ToolchainConfig) Tools() Tools {
	return Tools{Codex: c.Codex, GitHubCLI: c.GitHubCLI, Go: c.Go, NoMistakes: c.NoMistakes}
}
