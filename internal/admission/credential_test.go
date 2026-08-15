package admission

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/store"
)

type patVerificationStore struct{ value store.GitHubPATVerification }

func (s patVerificationStore) GitHubPATVerification(context.Context) (store.GitHubPATVerification, error) {
	return s.value, nil
}

func TestDynamicGitHubVerifierRereadsAndLiveVerifiesPATEveryAdmission(t *testing.T) {
	token := "github_pat_live"
	stored := store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "member", UserID: 7, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: `C:\Workflow\state\credentials\github.pat`, Status: "verified", VerifiedAt: time.Now().UTC()}
	reads, verifications := 0, 0
	verifier := DynamicGitHubVerifier{
		Store: patVerificationStore{value: stored},
		ReadPAT: func(context.Context, string) (string, error) {
			reads++
			if reads == 2 {
				return "", errors.New("credential deleted")
			}
			return token, nil
		},
		VerifyPAT: func(context.Context, string, string) (githubcredential.Verification, error) {
			verifications++
			return githubcredential.Verification{FingerprintSHA256: stored.FingerprintSHA256, Login: stored.Login, UserID: stored.UserID, Owner: stored.Owner, Scopes: stored.Scopes}, nil
		},
		VerifyRepository: func(context.Context, string, string, store.RepositoryAdmission, platformrelease.PlatformSetupContract) error {
			return nil
		},
	}
	value := store.RepositoryAdmission{Repository: "owner/one"}
	if err := verifier.Verify(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), value); err == nil || !strings.Contains(err.Error(), "read Control Plane PAT") {
		t.Fatalf("deleted credential error = %v", err)
	}
	if reads != 2 || verifications != 1 {
		t.Fatalf("reads=%d verifications=%d", reads, verifications)
	}
}

func TestDynamicGitHubVerifierRejectsFingerprintAndLiveSSODriftWithoutLeakingToken(t *testing.T) {
	token := "github_pat_secret_body"
	stored := store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint("old"), Login: "member", UserID: 7, Owner: "org", Scopes: []string{"repo", "workflow"}, CredentialPath: `C:\Workflow\state\credentials\github.pat`, Status: "verified", VerifiedAt: time.Now().UTC()}
	verifier := DynamicGitHubVerifier{Store: patVerificationStore{value: stored}, ReadPAT: func(context.Context, string) (string, error) { return token, nil }}
	err := verifier.Verify(context.Background(), store.RepositoryAdmission{Repository: "org/one"})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("fingerprint drift error = %v", err)
	}

	stored.FingerprintSHA256 = credential.Fingerprint(token)
	verifier.Store = patVerificationStore{value: stored}
	verifier.VerifyPAT = func(context.Context, string, string) (githubcredential.Verification, error) {
		return githubcredential.Verification{}, githubcredential.ErrSSOBlocked
	}
	err = verifier.Verify(context.Background(), store.RepositoryAdmission{Repository: "org/two"})
	if !errors.Is(err, githubcredential.ErrSSOBlocked) || strings.Contains(err.Error(), token) {
		t.Fatalf("SSO drift error = %v", err)
	}
}

func TestCredentialReplacementSuspendsEachRepositoryWithoutStoppingAdmissionPass(t *testing.T) {
	replacement := "github_pat_replacement_secret"
	stored := store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint("approved"), Login: "member", UserID: 7, Owner: "org", Scopes: []string{"repo", "workflow"}, CredentialPath: `C:\Workflow\state\credentials\github.pat`, Status: "verified", VerifiedAt: time.Now().UTC()}
	values := map[string]store.RepositoryAdmission{
		"org/one": {Repository: "org/one", Eligible: true},
		"org/two": {Repository: "org/two", Eligible: true},
	}
	service := Service{
		Store: &memoryStore{values: values},
		Verifier: DynamicGitHubVerifier{
			Store:   patVerificationStore{value: stored},
			ReadPAT: func(context.Context, string) (string, error) { return replacement, nil },
		},
	}
	if err := service.VerifyAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for repository, value := range values {
		if value.Eligible || !strings.Contains(value.SuspensionReason, "fingerprint") || strings.Contains(value.SuspensionReason, replacement) {
			t.Fatalf("%s admission after replacement = %#v", repository, value)
		}
	}
}

func TestOrganizationAdmissionSuspendsPendingApprovedScopeContract(t *testing.T) {
	token := "github_pat_org_admin_downgraded"
	stored := store.GitHubPATVerification{FingerprintSHA256: credential.Fingerprint(token), Login: "member", UserID: 7, Owner: "acme", Scopes: []string{"repo", "workflow"}, CredentialPath: `C:\Workflow\state\credentials\github.pat`, Status: "verified", VerifiedAt: time.Now().UTC()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
			_, _ = w.Write([]byte(`{"login":"member","id":7}`))
		case "/orgs/acme/memberships/member":
			_, _ = w.Write([]byte(`{"state":"active","role":"member"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	values := map[string]store.RepositoryAdmission{"acme/repo": {Repository: "acme/repo", Eligible: true}}
	service := Service{
		Store: &memoryStore{values: values},
		Verifier: DynamicGitHubVerifier{
			Store:      patVerificationStore{value: stored},
			APIBase:    server.URL,
			HTTPClient: server.Client(),
			ReadPAT:    func(context.Context, string) (string, error) { return token, nil },
			VerifyRepository: func(context.Context, string, string, store.RepositoryAdmission, platformrelease.PlatformSetupContract) error {
				return nil
			},
		},
	}
	if err := service.VerifyAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if values["acme/repo"].Eligible || !strings.Contains(values["acme/repo"].SuspensionReason, "approved organization scope contract") {
		t.Fatalf("downgraded owner admission = %#v", values["acme/repo"])
	}
}
