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

// Resolver locates the invoking Codex ChatGPT login through Codex's supported
// login status command and configuration home. The source override is owned by
// Agent Workflow for hosts that expose the supported login cache elsewhere.
type Resolver struct {
	LookupEnvironment func(string) string
	UserHomeDirectory func() (string, error)
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
		codexHome := strings.TrimSpace(lookup("CODEX_HOME"))
		if codexHome == "" {
			userHome := r.UserHomeDirectory
			if userHome == nil {
				userHome = os.UserHomeDir
			}
			resolved, err := userHome()
			if err != nil {
				return "", fmt.Errorf("resolve Codex configuration home: %w", err)
			}
			codexHome = filepath.Join(resolved, ".codex")
		}
		source = filepath.Join(codexHome, FileName)
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
