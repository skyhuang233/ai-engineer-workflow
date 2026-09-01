package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/onboarding"
)

func TestGitHubOnboardingRemoteCarriesHumanMergerIdentity(t *testing.T) {
	head := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[{"number":7}]`))
		case r.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"closed","merged_at":"2026-08-21T00:00:00Z","merge_commit_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","merged_by":{"login":"owner","type":"User"},"head":{"sha":"` + head + `","ref":"workflow/onboarding-digest"},"base":{"sha":"cccccccccccccccccccccccccccccccccccccccc","ref":"main"}}`))
		case r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/owner/repo/commits/"+head+"/check-runs":
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	remote := githubOnboardingRemote{client: workflowgithub.NewClient(server.URL, "token", server.Client()), owner: "owner"}
	pull, err := remote.OnboardingPull(context.Background(), "owner/repo", "workflow/onboarding-digest", "main", nil)
	if err != nil || pull.MergedBy != "owner" || pull.MergedByType != "User" {
		t.Fatalf("pull merger = %q/%q, err=%v", pull.MergedBy, pull.MergedByType, err)
	}
}

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
