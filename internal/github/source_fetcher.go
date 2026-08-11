package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

type DeliverySourceFetcher struct {
	Repository string
	Token      string
}

type deliverySourceAuthenticationError struct {
	err error
}

func (e deliverySourceAuthenticationError) Error() string             { return e.err.Error() }
func (e deliverySourceAuthenticationError) Unwrap() error             { return e.err }
func (deliverySourceAuthenticationError) AuthenticationFailure() bool { return true }

func (f DeliverySourceFetcher) Fetch(ctx context.Context, snapshotPath, headRef string) error {
	if err := ValidateRepository(f.Repository); err != nil {
		return err
	}
	if !filepath.IsAbs(snapshotPath) || f.Token == "" || !strings.HasPrefix(headRef, "refs/heads/") {
		return errors.New("credential-owning Delivery Source fetch adapter is incomplete")
	}
	branch := strings.TrimPrefix(headRef, "refs/heads/")
	if branch == "" {
		return errors.New("Delivery Source branch is required")
	}
	remoteURL := "https://github.com/" + f.Repository + ".git"
	endpoint, err := url.Parse(remoteURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return errors.New("credential-owning Delivery Source fetch adapter requires an HTTPS URL without embedded credentials")
	}
	return fetchDeliverySource(ctx, snapshotPath, remoteURL, headRef, &http.BasicAuth{Username: "x-access-token", Password: f.Token})
}

func fetchDeliverySource(ctx context.Context, snapshotPath, remoteURL, headRef string, auth transport.AuthMethod) error {
	repositoryStore, err := git.PlainOpen(snapshotPath)
	if err != nil {
		return fmt.Errorf("open Delivery Source snapshot: %w", err)
	}
	remote := git.NewRemote(repositoryStore.Storer, &config.RemoteConfig{Name: "workflow-source-refresh", URLs: []string{remoteURL}})
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("+" + headRef + ":" + headRef),
			config.RefSpec("+refs/tags/*:refs/tags/*"),
		},
		Auth: auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
			return deliverySourceAuthenticationError{err: fmt.Errorf("fetch admitted GitHub source: %w", err)}
		}
		return fmt.Errorf("fetch admitted GitHub source: %w", err)
	}
	return nil
}
