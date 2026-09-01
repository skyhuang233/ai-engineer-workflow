package executionauth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// ProbeAPI performs the sole API endpoint/key/model acceptance check. It uses
// an isolated CODEX_HOME and leaves no candidate credential in host Codex
// state.  The error intentionally omits command output because providers may
// echo a key in an upstream diagnostic.
func ProbeAPI(ctx context.Context, selection Selection) error {
	if selection.Mode != APIKey {
		return fmt.Errorf("API probe requires api_key mode")
	}
	if err := selection.Validate(); err != nil {
		return err
	}
	home, err := os.MkdirTemp("", "workflow-codex-api-probe-*")
	if err != nil {
		return fmt.Errorf("create isolated Codex probe home: %w", err)
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(ConfigPath(home), []byte(providerConfig(selection)), 0o600); err != nil {
		return fmt.Errorf("write isolated Codex probe configuration: %w", err)
	}
	command := exec.CommandContext(ctx, "codex", "exec", "--ephemeral", "--sandbox", "read-only", "--skip-git-repo-check", "Reply with OK.")
	command.Dir = home
	command.Env = append(os.Environ(), "CODEX_HOME="+home, APIKeyEnvironment+"="+selection.APIKey)
	if err := command.Run(); err != nil {
		return fmt.Errorf("validate API execution credentials with isolated Codex probe: %w", err)
	}
	return nil
}
