package codexauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const SourceOverrideEnvironment = "WORKFLOW_CODEX_AUTH_FILE"

// Resolver validates the invoking Codex ChatGPT login through Codex's supported
// status command and an explicit source supplied by the Codex integration.
// Codex does not expose a supported command for discovering its private source.
type Resolver struct {
	LookupEnvironment func(string) string
	LoginStatus       func(context.Context) ([]byte, error)
}

func ResolveChatGPT(ctx context.Context) (string, error) {
	return (Resolver{}).ResolveChatGPT(ctx)
}

func (r Resolver) ResolveChatGPT(ctx context.Context) (string, error) {
	lookup := r.LookupEnvironment
	if lookup == nil {
		lookup = os.Getenv
	}
	status := r.LoginStatus
	if status == nil {
		status = func(ctx context.Context) ([]byte, error) {
			output, err := exec.CommandContext(ctx, "codex", "login", "status").CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("query Codex login status: %w", err)
			}
			return output, nil
		}
	}
	output, err := status(ctx)
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToLower(string(output)), "logged in using chatgpt") {
		return "", errors.New("Codex must be logged in using ChatGPT")
	}

	source := strings.TrimSpace(lookup(SourceOverrideEnvironment))
	if source == "" {
		return "", fmt.Errorf("Codex does not expose its credential source through a supported CLI interface; set %s to the absolute ChatGPT authentication source supplied by the invoking Codex integration", SourceOverrideEnvironment)
	}
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("%s must be an absolute path supplied by the invoking Codex integration", SourceOverrideEnvironment)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve Codex authentication source: %w", err)
	}
	if err := ValidateChatGPT(source); err != nil {
		return "", err
	}
	return filepath.Clean(source), nil
}
