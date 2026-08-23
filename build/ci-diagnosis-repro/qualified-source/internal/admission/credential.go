package admission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/store"
)

type PATVerificationStore interface {
	GitHubPATVerification(context.Context) (store.GitHubPATVerification, error)
}

type DynamicGitHubVerifier struct {
	Store            PATVerificationStore
	Contract         RepositoryContract
	APIBase          string
	HTTPClient       *http.Client
	ReadPAT          func(context.Context, string) (string, error)
	VerifyPAT        func(context.Context, string, string) (githubcredential.Verification, error)
	VerifyRepository func(context.Context, string, string, store.RepositoryAdmission, RepositoryContract) error
}

// Verify deliberately rereads and revalidates the plaintext PAT for every
// Repository Admission. No token body is retained in the verifier or errors.
func (v DynamicGitHubVerifier) Verify(ctx context.Context, value store.RepositoryAdmission) error {
	if v.Store == nil {
		return errors.New("Control Plane PAT verification store is unavailable")
	}
	recorded, err := v.Store.GitHubPATVerification(ctx)
	if err != nil || recorded.Status != "verified" {
		return errors.Join(errors.New("Control Plane PAT verification is unavailable"), err)
	}
	read := v.ReadPAT
	if read == nil {
		read = func(ctx context.Context, path string) (string, error) {
			return credential.NewFileStore(path).Get(ctx, credential.GatewayTarget)
		}
	}
	token, err := read(ctx, recorded.CredentialPath)
	if err != nil {
		return fmt.Errorf("read Control Plane PAT: %w", err)
	}
	if credential.Fingerprint(token) != recorded.FingerprintSHA256 {
		return errors.New("Control Plane PAT fingerprint differs from its verified record")
	}
	verify := v.VerifyPAT
	if verify == nil {
		verifier := githubcredential.Verifier{APIBase: v.APIBase, Client: v.HTTPClient}
		verify = verifier.Verify
	}
	live, err := verify(ctx, token, recorded.Owner)
	if err != nil {
		return fmt.Errorf("live verify Control Plane PAT: %w", err)
	}
	if live.FingerprintSHA256 != recorded.FingerprintSHA256 || live.UserID != recorded.UserID || !strings.EqualFold(live.Login, recorded.Login) || !strings.EqualFold(live.Owner, recorded.Owner) {
		return errors.New("Control Plane PAT live identity differs from its verified record")
	}
	for _, required := range v.Contract.RequiredScopes {
		if !containsFold(live.Scopes, required) {
			return errors.New("Control Plane PAT live scopes differ from the installed platform contract")
		}
	}
	verifyRepository := v.VerifyRepository
	if verifyRepository == nil {
		verifyRepository = func(ctx context.Context, token, owner string, value store.RepositoryAdmission, contract RepositoryContract) error {
			client := github.NewClient(v.APIBase, token, v.HTTPClient).WithRepositoryOwner(owner)
			return (GitHubVerifier{Client: client, Contract: contract}).Verify(ctx, value)
		}
	}
	return verifyRepository(ctx, token, recorded.Owner, value, v.Contract)
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}
