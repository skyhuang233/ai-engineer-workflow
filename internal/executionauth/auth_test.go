package executionauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memoryEnvironmentStore struct {
	values map[string]string
	failAt string
}

func (s *memoryEnvironmentStore) Load(name string) (string, error) { return s.values[name], nil }
func (s *memoryEnvironmentStore) Save(name, value string) error {
	if name == s.failAt {
		return errors.New("injected persistence failure")
	}
	s.values[name] = value
	return nil
}

func TestCommitAPISelectionIsAtomicAndActivatesOnlyAfterPersistence(t *testing.T) {
	store := &memoryEnvironmentStore{values: map[string]string{ModeEnvironment: string(CodexLogin), BaseURLEnvironment: "old-url", APIKeyEnvironment: "old-key", ModelEnvironment: "old-model"}, failAt: ModelEnvironment}
	t.Setenv(ModeEnvironment, "inherited")
	selection := Selection{Mode: APIKey, BaseURL: "https://example.test/v1", APIKey: "candidate-key", Model: "candidate-model"}
	if err := Commit(store, selection); err == nil {
		t.Fatal("Commit accepted an injected persistence failure")
	}
	if got := store.values; got[ModeEnvironment] != string(CodexLogin) || got[BaseURLEnvironment] != "old-url" || got[APIKeyEnvironment] != "old-key" || got[ModelEnvironment] != "old-model" {
		t.Fatalf("failed API update persisted partial state: %#v", got)
	}
	if got := os.Getenv(ModeEnvironment); got != "inherited" {
		t.Fatalf("failed API update activated process mode %q", got)
	}
	store.failAt = ""
	if err := Commit(store, selection); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(APIKeyEnvironment); got != "candidate-key" {
		t.Fatalf("successful API update did not activate current key: %q", got)
	}
}

func TestCommitCodexLoginRetainsAPIValues(t *testing.T) {
	store := &memoryEnvironmentStore{values: map[string]string{ModeEnvironment: string(APIKey), BaseURLEnvironment: "https://example.test/v1", APIKeyEnvironment: "key", ModelEnvironment: "model"}}
	source := filepath.Join(t.TempDir(), "auth.json")
	writeChatGPTCache(t, source)
	if err := Commit(store, Selection{Mode: CodexLogin, CodexAuthFile: source}); err != nil {
		t.Fatal(err)
	}
	if store.values[ModeEnvironment] != string(CodexLogin) || store.values[BaseURLEnvironment] != "https://example.test/v1" || store.values[APIKeyEnvironment] != "key" || store.values[ModelEnvironment] != "model" {
		t.Fatalf("Codex Login switch discarded API configuration: %#v", store.values)
	}
}

func TestReloadClearsInheritedValuesMissingFromRegistry(t *testing.T) {
	store := &memoryEnvironmentStore{values: map[string]string{ModeEnvironment: string(CodexLogin)}}
	t.Setenv(APIKeyEnvironment, "inherited-key")
	selection, err := Reload(store)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Mode != CodexLogin || os.Getenv(APIKeyEnvironment) != "" {
		t.Fatalf("registry reload did not replace inherited environment: %#v key=%q", selection, os.Getenv(APIKeyEnvironment))
	}
}

func TestSessionSnapshotsDoNotPersistAPIKeyAndRemainStable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	selection := Selection{Mode: APIKey, BaseURL: "https://example.test/v1", APIKey: "do-not-persist", Model: "model-a"}
	if err := WriteNewSession(selection, home); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{SessionStatePath(home), ConfigPath(home)} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), selection.APIKey) {
			t.Fatalf("API key was persisted in %s", path)
		}
	}
	got, err := ReadSession(home)
	if err != nil || got.Mode != APIKey || got.BaseURL != selection.BaseURL || got.Model != selection.Model || got.APIKey != "" {
		t.Fatalf("API session snapshot = %#v, %v", got, err)
	}
	if err := WriteNewSession(selection, home); err == nil {
		t.Fatal("new API Session overwrote the existing snapshot")
	}
}

func TestResolveCurrentSelectionRequiresExplicitMode(t *testing.T) {
	t.Setenv(ModeEnvironment, "")
	if _, err := ResolveCurrentSelection(context.Background(), nil); err == nil {
		t.Fatal("ambient API variables selected a mode")
	}
}

func writeChatGPTCache(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account","id_token":"id","refresh_token":"refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}
