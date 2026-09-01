package main

import (
	"context"
	"testing"

	"github.com/skyhuang233/workflow/internal/executionauth"
)

func TestParseGitHubRemote(t *testing.T) {
	for _, test := range []struct{ remote, want string }{
		{"git@github.com:owner/repository.git", "owner/repository"},
		{"https://github.com/owner/repository.git", "owner/repository"},
		{"ssh://git@github.com/owner/repository", "owner/repository"},
	} {
		address, ok := parseGitHubRemote(test.remote)
		if !ok || address.String() != test.want {
			t.Fatalf("parse %q = %q, %v", test.remote, address.String(), ok)
		}
	}
	if _, ok := parseGitHubRemote("https://example.com/owner/repository"); ok {
		t.Fatal("non-GitHub remote accepted")
	}
}

func TestExecutionAuthenticationRecognizesConfiguredAPIKey(t *testing.T) {
	t.Setenv(executionauth.ModeEnvironment, string(executionauth.APIKey))
	t.Setenv(executionauth.BaseURLEnvironment, "https://api.example.test/v1")
	t.Setenv(executionauth.APIKeyEnvironment, "test-api-key")
	t.Setenv(executionauth.ModelEnvironment, "test-model")
	mode, ready, err := (executionAuthentication{}).Ready(context.Background())
	if err != nil || !ready || mode != string(executionauth.APIKey) {
		t.Fatalf("authentication readiness = %q, %t, %v", mode, ready, err)
	}
}
