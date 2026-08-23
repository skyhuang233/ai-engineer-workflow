package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolverUsesMachineReadableCodexDoctorAuthenticationPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex")
	source := filepath.Join(home, FileName)
	writeChatGPTCache(t, source)
	resolver := Resolver{
		LookupEnvironment: func(string) string { return "" },
		Doctor: func(context.Context) ([]byte, error) {
			return doctorReportJSON(t, home, source, "true", "chatgpt"), nil
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT\n"), nil },
	}
	if got, err := resolver.ResolveChatGPT(context.Background()); err != nil || got != source {
		t.Fatalf("doctor source = %q, %v", got, err)
	}
}

func TestResolverAcceptsRequiredDoctorChecksWhenCommandReportsOtherFailures(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex")
	source := filepath.Join(home, FileName)
	writeChatGPTCache(t, source)
	resolver := Resolver{
		LookupEnvironment: func(string) string { return "" },
		Doctor: func(context.Context) ([]byte, error) {
			return doctorReportJSON(t, home, source, "true", "chatgpt"), errors.New("exit status 1")
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT\n"), nil },
	}
	if got, err := resolver.ResolveChatGPT(context.Background()); err != nil || got != source {
		t.Fatalf("valid required checks from nonzero doctor = %q, %v", got, err)
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
		Doctor: func(context.Context) ([]byte, error) {
			return doctorReportJSON(t, filepath.Dir(source), source, "true", "chatgpt"), nil
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT"), nil },
	}
	got, err := resolver.ResolveChatGPT(context.Background())
	if err != nil || got != source {
		t.Fatalf("resolved source = %q, %v", got, err)
	}
}

func TestResolverRejectsRelativeIntegrationSource(t *testing.T) {
	working := t.TempDir()
	writeChatGPTCache(t, filepath.Join(working, "private-auth.json"))
	t.Chdir(working)
	resolver := Resolver{
		LookupEnvironment: func(name string) string {
			if name == SourceOverrideEnvironment {
				return "private-auth.json"
			}
			return ""
		},
		Doctor: func(context.Context) ([]byte, error) {
			return doctorReportJSON(t, working, filepath.Join(working, FileName), "true", "chatgpt"), nil
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT"), nil },
	}
	if got, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatalf("relative private source was resolved as %q", got)
	}
}

func TestResolverRejectsNonChatGPTOrInvalidCache(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, FileName)
	writeChatGPTCache(t, source)
	resolver := Resolver{
		LookupEnvironment: func(string) string { return "" },
		Doctor: func(context.Context) ([]byte, error) {
			return doctorReportJSON(t, home, source, "true", "chatgpt"), nil
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using an API key"), nil },
	}
	if _, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatal("non-ChatGPT login was accepted")
	}
	resolver.LoginStatus = func(context.Context) ([]byte, error) { return nil, errors.New("not logged in") }
	if _, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatal("failed login status was accepted")
	}
}

func TestResolverRejectsIncompleteOrNonChatGPTDoctorCapability(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, FileName)
	writeChatGPTCache(t, source)
	resolver := Resolver{
		LookupEnvironment: func(string) string { return "" },
		Doctor: func(context.Context) ([]byte, error) {
			return doctorReportJSON(t, home, source, "false", "chatgpt"), nil
		},
		LoginStatus: func(context.Context) ([]byte, error) { return []byte("Logged in using ChatGPT"), nil },
	}
	if _, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatal("doctor without stored ChatGPT tokens was accepted")
	}
	resolver.Doctor = func(context.Context) ([]byte, error) { return []byte(`{"schemaVersion":2,"checks":{}}`), nil }
	if _, err := resolver.ResolveChatGPT(context.Background()); err == nil {
		t.Fatal("unsupported doctor schema was accepted")
	}
}

func doctorReportJSON(t *testing.T, home, source, tokens, mode string) []byte {
	t.Helper()
	report := map[string]any{
		"schemaVersion": 1,
		"codexVersion":  "0.147.0",
		"checks": map[string]any{
			"auth.credentials": map[string]any{"status": "ok", "details": map[string]string{"auth file": source, "stored ChatGPT tokens": tokens, "stored auth mode": mode}},
			"config.load":      map[string]any{"status": "ok", "details": map[string]string{"CODEX_HOME": home}},
		},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
