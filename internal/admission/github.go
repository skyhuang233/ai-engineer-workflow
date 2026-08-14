package admission

import (
	"context"
	"errors"
	"strings"

	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/store"
)

// GitHubVerifier performs the lightweight recurring remote checks that fence
// new work. It is non-mutating; repair remains an explicit Setup operation.
type GitHubVerifier struct {
	Client   *github.Client
	Contract platformrelease.PlatformSetupContract
}

func (v GitHubVerifier) Verify(ctx context.Context, value store.RepositoryAdmission) error {
	if v.Client == nil {
		return errors.New("Repository Admission GitHub client is unavailable")
	}
	repository, err := v.Client.RepositoryForOnboarding(ctx, value.Repository)
	if err != nil {
		return err
	}
	manifest, err := repositorycontract.VerifyRemote(func(path string) ([]byte, error) {
		return v.Client.RepositoryFile(ctx, value.Repository, path, repository.DefaultBranch)
	}, value.Repository, repository.DefaultBranch, value.ManifestDigestSHA256)
	if err != nil {
		return err
	}
	if manifest.ContractVersion != value.ContractVersion || manifest.ContractVersion != v.Contract.RepositoryContract.Version {
		return errors.New("Repository Contract version differs from the installed platform")
	}
	for _, expected := range v.Contract.RepositoryContract.Labels {
		actual, labelErr := v.Client.Label(ctx, value.Repository, expected.Name)
		if labelErr != nil {
			return labelErr
		}
		if !strings.EqualFold(actual.Color, expected.Color) || actual.Description != expected.Description {
			return errors.New("managed GitHub label differs: " + expected.Name)
		}
	}
	policy, err := v.Client.DiscoverPolicy(ctx, value.Repository, repository.DefaultBranch)
	if err != nil {
		return err
	}
	if !policy.HasIssues || !policy.ActionsEnabled {
		return errors.New("GitHub Issues or Actions is unavailable")
	}
	return nil
}
