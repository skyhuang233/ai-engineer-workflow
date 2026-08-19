package workflowrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type BuildInput struct {
	SchemaVersion int         `json:"schema_version"`
	GitInputs     GitInputs   `json:"git_inputs"`
	Toolchain     Tools       `json:"toolchain"`
	Worker        BuildWorker `json:"worker"`
}

type GitInputs struct {
	DeployWorkerTree         string `json:"deploy_worker_tree"`
	DeliverySourceDigestTree string `json:"delivery_source_digest_tree"`
	DeliverySourceTree       string `json:"delivery_source_tree"`
	GoModBlob                string `json:"go_mod_blob"`
	GoSumBlob                string `json:"go_sum_blob"`
	PublishWorkflowBlob      string `json:"publish_workflow_blob"`
}

type BuildWorker struct {
	ImageRepository string `json:"image_repository"`
}

func DecodeBuildInput(raw []byte) (BuildInput, error) {
	var input BuildInput
	if err := decodeStrict(raw, &input); err != nil {
		return BuildInput{}, fmt.Errorf("decode Worker build input: %w", err)
	}
	if err := input.Validate(); err != nil {
		return BuildInput{}, err
	}
	return input, nil
}

func (i BuildInput) Validate() error {
	if i.SchemaVersion != 1 {
		return errors.New("unsupported Worker build-input schema")
	}
	for name, oid := range map[string]string{
		"deploy Worker tree":          i.GitInputs.DeployWorkerTree,
		"delivery-source-digest tree": i.GitInputs.DeliverySourceDigestTree,
		"delivery source tree":        i.GitInputs.DeliverySourceTree,
		"go.mod blob":                 i.GitInputs.GoModBlob,
		"go.sum blob":                 i.GitInputs.GoSumBlob,
		"publish workflow blob":       i.GitInputs.PublishWorkflowBlob,
	} {
		if !hex40Pattern.MatchString(oid) {
			return fmt.Errorf("%s must be a lowercase 40-character Git object ID", name)
		}
	}
	if err := i.Toolchain.Validate(); err != nil {
		return err
	}
	if i.Worker.ImageRepository != WorkerRepository {
		return fmt.Errorf("Worker image repository must be %s", WorkerRepository)
	}
	return nil
}

func (i BuildInput) Canonical() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(i)
}

func (i BuildInput) Identity() (string, error) {
	canonical, err := i.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

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
		return ToolchainConfig{}, errors.New("Worker image release repository is invalid")
	}
	return config, nil
}

func (c ToolchainConfig) Tools() Tools {
	return Tools{Codex: c.Codex, GitHubCLI: c.GitHubCLI, Go: c.Go, NoMistakes: c.NoMistakes}
}
