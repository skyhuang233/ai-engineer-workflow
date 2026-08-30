// Package executionauth owns the host-scoped credential selection used by
// Worker Ticket Sessions.  It deliberately keeps the selected mode outside
// repository runtime configuration and never guesses it from ambient state.
package executionauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/skyhuang233/workflow/internal/codexauth"
)

const (
	ModeEnvironment    = "WORKFLOW_EXECUTION_AUTH_MODE"
	BaseURLEnvironment = "WORKFLOW_OPENAI_BASE_URL"
	APIKeyEnvironment  = "WORKFLOW_OPENAI_API_KEY"
	ModelEnvironment   = "WORKFLOW_OPENAI_MODEL"

	stateFileName  = "workflow-execution-auth.json"
	configFileName = "config.toml"
)

type Mode string

const (
	APIKey     Mode = "api_key"
	CodexLogin Mode = "codex_login"
)

// Selection contains the current host selection. APIKey is intentionally
// never written to a Ticket Session; it is supplied for every API-mode Run.
type Selection struct {
	Mode          Mode
	BaseURL       string
	APIKey        string
	Model         string
	CodexAuthFile string
}

func (s Selection) Validate() error {
	switch s.Mode {
	case APIKey:
		if strings.TrimSpace(s.BaseURL) == "" || strings.TrimSpace(s.APIKey) == "" || strings.TrimSpace(s.Model) == "" {
			return errors.New("API-key execution requires endpoint, API key, and model")
		}
	case CodexLogin:
		if strings.TrimSpace(s.CodexAuthFile) == "" || !filepath.IsAbs(s.CodexAuthFile) {
			return errors.New("Codex-login execution requires a verified absolute Codex authentication source")
		}
	default:
		return errors.New("Worker execution authentication mode must be api_key or codex_login")
	}
	return nil
}

// EnvironmentStore is the narrow persistence boundary for HKCU\Environment.
// Tests use a memory implementation; Windows supplies the real implementation.
type EnvironmentStore interface {
	Load(string) (string, error)
	Save(string, string) error
}

func names() []string {
	return []string{ModeEnvironment, BaseURLEnvironment, APIKeyEnvironment, ModelEnvironment}
}

func Load(store EnvironmentStore) (Selection, error) {
	if store == nil {
		return Selection{}, errors.New("Worker execution authentication environment store is required")
	}
	values := make(map[string]string, len(names()))
	for _, name := range names() {
		value, err := store.Load(name)
		if err != nil {
			return Selection{}, fmt.Errorf("load %s: %w", name, err)
		}
		values[name] = strings.TrimSpace(value)
	}
	return Selection{Mode: Mode(values[ModeEnvironment]), BaseURL: values[BaseURLEnvironment], APIKey: values[APIKeyEnvironment], Model: values[ModelEnvironment]}, nil
}

// Commit persists a successful explicit selection. A failed API write restores
// every prior value and never mutates this process's environment.
func Commit(store EnvironmentStore, selection Selection) error {
	if store == nil {
		return errors.New("Worker execution authentication environment store is required")
	}
	if err := selection.Validate(); err != nil {
		return err
	}
	previous := make(map[string]string, len(names()))
	for _, name := range names() {
		value, err := store.Load(name)
		if err != nil {
			return fmt.Errorf("read current %s: %w", name, err)
		}
		previous[name] = value
	}
	values := map[string]string{ModeEnvironment: string(selection.Mode)}
	if selection.Mode == APIKey {
		values[BaseURLEnvironment] = selection.BaseURL
		values[APIKeyEnvironment] = selection.APIKey
		values[ModelEnvironment] = selection.Model
	}
	changed := make([]string, 0, len(values))
	for _, name := range names() {
		value, ok := values[name]
		if !ok || previous[name] == value {
			continue
		}
		if err := store.Save(name, value); err != nil {
			for index := len(changed) - 1; index >= 0; index-- {
				_ = store.Save(changed[index], previous[changed[index]])
			}
			return fmt.Errorf("persist %s: %w", name, err)
		}
		changed = append(changed, name)
	}
	for _, name := range names() {
		if value, ok := values[name]; ok {
			if err := os.Setenv(name, value); err != nil {
				return fmt.Errorf("activate %s: %w", name, err)
			}
		}
	}
	return nil
}

// Reload makes HKCU authoritative at the process restart boundary. Empty
// persisted values actively clear inherited launcher values.
func Reload(store EnvironmentStore) (Selection, error) {
	selection, err := Load(store)
	if err != nil {
		return Selection{}, err
	}
	values := map[string]string{ModeEnvironment: string(selection.Mode), BaseURLEnvironment: selection.BaseURL, APIKeyEnvironment: selection.APIKey, ModelEnvironment: selection.Model}
	for _, name := range names() {
		if values[name] == "" {
			if err := os.Unsetenv(name); err != nil {
				return Selection{}, fmt.Errorf("clear inherited %s: %w", name, err)
			}
		} else if err := os.Setenv(name, values[name]); err != nil {
			return Selection{}, fmt.Errorf("activate %s: %w", name, err)
		}
	}
	return selection, nil
}

// CurrentProcessSelection reads the selected mode after the startup reload
// boundary. It deliberately does not query Codex Login: existing API Sessions
// must remain resumable if a later login selection is not currently ready.
func CurrentProcessSelection() (Selection, error) {
	selection := Selection{Mode: Mode(strings.TrimSpace(os.Getenv(ModeEnvironment))), BaseURL: strings.TrimSpace(os.Getenv(BaseURLEnvironment)), APIKey: strings.TrimSpace(os.Getenv(APIKeyEnvironment)), Model: strings.TrimSpace(os.Getenv(ModelEnvironment))}
	if selection.Mode == CodexLogin {
		return selection, nil
	}
	if err := selection.Validate(); err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// ResolveCurrentSelection resolves a currently ready selection for operations
// such as runtime configuration. Codex Login verifies its doctor-verified
// source only at this explicit readiness boundary.
func ResolveCurrentSelection(ctx context.Context, resolveChatGPT func(context.Context) (string, error)) (Selection, error) {
	selection, err := CurrentProcessSelection()
	if err != nil {
		return Selection{}, err
	}
	if selection.Mode == CodexLogin {
		if resolveChatGPT == nil {
			resolveChatGPT = codexauth.ResolveDoctorVerifiedChatGPT
		}
		source, err := resolveChatGPT(ctx)
		if err != nil {
			return Selection{}, fmt.Errorf("Codex Login Execution is not ready; run codex login outside Setup: %w", err)
		}
		selection.CodexAuthFile = source
	}
	if err := selection.Validate(); err != nil {
		return Selection{}, err
	}
	return selection, nil
}

type sessionState struct {
	Mode    Mode   `json:"mode"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
}

func SessionStatePath(codexHome string) string { return filepath.Join(codexHome, stateFileName) }

func ConfigPath(codexHome string) string { return filepath.Join(codexHome, configFileName) }

func WriteNewSession(selection Selection, codexHome string) error {
	if err := selection.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(codexHome) {
		return errors.New("Ticket Session Codex state must be an absolute path")
	}
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return fmt.Errorf("create Ticket Session Codex state: %w", err)
	}
	state := sessionState{Mode: selection.Mode}
	if selection.Mode == APIKey {
		state.BaseURL, state.Model = selection.BaseURL, selection.Model
		if err := writeNewFile(ConfigPath(codexHome), []byte(providerConfig(selection)), 0o600); err != nil {
			return err
		}
	} else if err := codexauth.SeedNew(selection.CodexAuthFile, codexHome); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeNewFile(SessionStatePath(codexHome), raw, 0o600)
}

func ReadSession(codexHome string) (Selection, error) {
	raw, err := os.ReadFile(SessionStatePath(codexHome))
	if errors.Is(err, os.ErrNotExist) {
		// Sessions created before explicit modes are valid ChatGPT snapshots.
		if legacyErr := codexauth.ValidateChatGPT(filepath.Join(codexHome, codexauth.FileName)); legacyErr == nil {
			return Selection{Mode: CodexLogin, CodexAuthFile: filepath.Join(codexHome, codexauth.FileName)}, nil
		}
		return Selection{}, errors.New("Ticket Session Worker execution authentication snapshot is unavailable")
	}
	if err != nil {
		return Selection{}, fmt.Errorf("read Ticket Session Worker execution authentication snapshot: %w", err)
	}
	var state sessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return Selection{}, errors.New("Ticket Session Worker execution authentication snapshot is invalid")
	}
	selection := Selection{Mode: state.Mode, BaseURL: state.BaseURL, Model: state.Model}
	switch selection.Mode {
	case APIKey:
		if strings.TrimSpace(selection.BaseURL) == "" || strings.TrimSpace(selection.Model) == "" {
			return Selection{}, errors.New("Ticket Session API execution snapshot is incomplete")
		}
		if _, err := os.Stat(ConfigPath(codexHome)); err != nil {
			return Selection{}, errors.New("Ticket Session API provider configuration is unavailable")
		}
	case CodexLogin:
		selection.CodexAuthFile = filepath.Join(codexHome, codexauth.FileName)
		if err := codexauth.ValidateChatGPT(selection.CodexAuthFile); err != nil {
			return Selection{}, errors.New("Ticket Session Codex authentication cache is unavailable")
		}
	default:
		return Selection{}, errors.New("Ticket Session Worker execution authentication mode is invalid")
	}
	return selection, nil
}

func providerConfig(selection Selection) string {
	return fmt.Sprintf("model = %q\nmodel_provider = %q\n\n[model_providers.workflow]\nbase_url = %q\nenv_key = %q\n", selection.Model, "workflow", selection.BaseURL, APIKeyEnvironment)
}

func writeNewFile(path string, content []byte, mode os.FileMode) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("new Ticket Session file already exists: %s", filepath.Base(path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
