package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/onboarding"
)

type onboardingRoundTripper func(*http.Request) (*http.Response, error)

func (f onboardingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGitHubOnboardingRemoteMapsOnlyTyped404ToResourceNotFound(t *testing.T) {
	resources := []struct {
		name string
		call func(githubOnboardingRemote) error
		want error
	}{
		{name: "label", call: func(r githubOnboardingRemote) error {
			_, err := r.Label(context.Background(), "owner/repo", "workflow")
			return err
		}, want: onboarding.ErrManagedLabelNotFound},
		{name: "features", call: func(r githubOnboardingRemote) error {
			_, _, _, err := r.Features(context.Background(), "owner/repo")
			return err
		}, want: onboarding.ErrRepositoryNotFound},
		{name: "variable", call: func(r githubOnboardingRemote) error {
			_, err := r.Variable(context.Background(), "owner/repo", "WORKFLOW")
			return err
		}, want: onboarding.ErrRepositoryVariableNotFound},
		{name: "managed content", call: func(r githubOnboardingRemote) error {
			return r.VerifyOnboardingContent(context.Background(), "owner/repo", "main", map[string][]byte{"AGENTS.md": []byte("managed")})
		}, want: onboarding.ErrManagedContentNotFound},
		{name: "contract", call: func(r githubOnboardingRemote) error {
			return r.VerifyContract(context.Background(), "owner/repo", "main", "digest")
		}, want: onboarding.ErrRepositoryContractNotFound},
	}
	for _, resource := range resources {
		t.Run(resource.name, func(t *testing.T) {
			for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "failure", status) }))
					defer server.Close()
					remote := githubOnboardingRemote{client: workflowgithub.NewClient(server.URL, "token", nil), owner: "owner"}
					err := resource.call(remote)
					if status == http.StatusNotFound {
						if !errors.Is(err, resource.want) {
							t.Fatalf("404 err=%v, want %v", err, resource.want)
						}
						return
					}
					if err == nil || errors.Is(err, resource.want) {
						t.Fatalf("status %d incorrectly mapped as missing: %v", status, err)
					}
				})
			}
			t.Run("network", func(t *testing.T) {
				remote := githubOnboardingRemote{client: workflowgithub.NewClient("https://example.invalid", "token", &http.Client{Transport: onboardingRoundTripper(func(*http.Request) (*http.Response, error) {
					return nil, context.DeadlineExceeded
				})}), owner: "owner"}
				err := resource.call(remote)
				if err == nil || errors.Is(err, resource.want) {
					t.Fatalf("network error incorrectly mapped as missing: %v", err)
				}
			})
		})
	}
}
