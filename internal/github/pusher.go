package github

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

type WorkspacePusher struct {
	WorkspacePath string
	Token         string
	PushURL       string
}

func (p WorkspacePusher) Push(ctx context.Context, repository, branch, commit, expectedHead string, expectAbsent bool) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	if !filepath.IsAbs(p.WorkspacePath) || p.Token == "" || branch == "" || commit == "" {
		return errors.New("credential-owning push adapter is incomplete")
	}
	pushURL := p.PushURL
	canonicalURL := "https://github.com/" + repository + ".git"
	if pushURL == "" {
		pushURL = canonicalURL
	}
	endpoint, err := url.Parse(pushURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return errors.New("credential-owning push adapter requires an HTTPS push URL without embedded credentials")
	}
	if pushURL != canonicalURL {
		return errors.New("credential-owning push adapter requires the admitted GitHub repository push URL")
	}
	repositoryStore, err := git.PlainOpen(p.WorkspacePath)
	if err != nil {
		return fmt.Errorf("open candidate workspace: %w", err)
	}
	refID := fmt.Sprintf("%x", sha256.Sum256([]byte(branch+"\x00"+commit)))
	localRef := plumbing.ReferenceName("refs/workflow-gateway/" + refID)
	remoteRef := plumbing.NewBranchReferenceName(branch)
	trackingRef := gatewayLeaseTrackingRef(localRef)
	if err := repositoryStore.Storer.SetReference(plumbing.NewHashReference(localRef, plumbing.NewHash(commit))); err != nil {
		return fmt.Errorf("stage candidate ref: %w", err)
	}
	defer repositoryStore.Storer.RemoveReference(localRef)
	expected := plumbing.ZeroHash
	if !expectAbsent {
		expected = plumbing.NewHash(expectedHead)
	}
	if err := repositoryStore.Storer.SetReference(plumbing.NewHashReference(trackingRef, expected)); err != nil {
		return fmt.Errorf("stage remote lease: %w", err)
	}
	defer repositoryStore.Storer.RemoveReference(trackingRef)

	remote := git.NewRemote(repositoryStore.Storer, &config.RemoteConfig{Name: "workflow-gateway", URLs: []string{pushURL}})
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: "workflow-gateway",
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + localRef.String() + ":" + remoteRef.String())},
		Auth:       &http.BasicAuth{Username: "x-access-token", Password: p.Token},
		ForceWithLease: &git.ForceWithLease{
			RefName: remoteRef,
			Hash:    expected,
		},
	})
	if err != nil {
		return fmt.Errorf("push candidate branch: %w", err)
	}
	return nil
}

func gatewayLeaseTrackingRef(localRef plumbing.ReferenceName) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/remotes/workflow-gateway/" + localRef.String())
}
