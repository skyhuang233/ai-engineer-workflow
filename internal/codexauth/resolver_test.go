package codexauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolverUsesSupportedCodexHomeAfterChatGPTStatus(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex")
	writeChatGPTCache(t, filepath.Join(home, FileName))
	resolver := Resolver{
		LookupEnvironment: func(name string) string {
			if name == "CODEX_HOME" {
				return home
			}
			return ""
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT\n"), nil },
	}
	got, err := resolver.ResolveChatGPT(context.Background())
	if err != nil || got != filepath.Join(home, FileName) {
		t.Fatalf("resolved source = %q, %v", got, err)
	}
}

func TestResolverSupportsExplicitWorkflowIntegrationSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "supported-login.json")
	writeChatGPTCache(t, source)
	resolver := Resolver{
		LookupEnvironment: func(name string) string {
			if name == SourceOverrideEnvironment {
				return source
			}
			return ""
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT"), nil },
	}
	got, err := resolver.ResolveChatGPT(context.Background())
	if err != nil || got != source {
		t.Fatalf("resolved source = %q, %v", got, err)
	}
}

func TestResolverRejectsNonChatGPTOrInvalidCache(t *testing.T) {
	resolver := Resolver{
		LookupEnvironment: func(string) string { return filepath.Join(t.TempDir(), "missing") },
		LoginStatus:       func(context.Context) ([]byte, error) { return []byte("Logged in using an API key"), nil },
	}
	if _, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatal("non-ChatGPT login was accepted")
	}
	resolver.LoginStatus = func(context.Context) ([]byte, error) { return nil, errors.New("not logged in") }
	if _, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatal("failed login status was accepted")
	}
}

func writeChatGPTCache(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account","id_token":"id","refresh_token":"refresh"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
