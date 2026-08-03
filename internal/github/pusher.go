package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
)

type WorkspacePusher struct {
	WorkspacePath string
	Token         string
	PushURL       string
	Binary        string
}

func (p WorkspacePusher) Push(ctx context.Context, repository, branch, commit, expectedHead string, expectAbsent bool) error {
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	if !filepath.IsAbs(p.WorkspacePath) || p.Token == "" || branch == "" || commit == "" {
		return errors.New("credential-owning push adapter is incomplete")
	}
	pushURL := p.PushURL
	if pushURL == "" {
		pushURL = "https://github.com/" + repository + ".git"
	}
	lease := "refs/heads/" + branch + ":" + expectedHead
	if expectAbsent {
		lease = "refs/heads/" + branch + ":"
	}
	binary := p.Binary
	if binary == "" {
		binary = "git"
	}
	credentialStore, err := p.createCredentialStore(pushURL)
	if err != nil {
		return err
	}
	defer os.Remove(credentialStore)
	cmd := pushCommand(ctx, binary, p.WorkspacePath, credentialStore, lease, pushURL, commit, branch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push candidate branch: %w (%s)", err, string(output))
	}
	return nil
}

func (p WorkspacePusher) createCredentialStore(pushURL string) (string, error) {
	endpoint, err := url.Parse(pushURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return "", errors.New("credential-owning push adapter requires an HTTPS push URL")
	}
	credentialStore, err := os.CreateTemp(p.WorkspacePath, ".workflow-git-credential-*")
	if err != nil {
		return "", fmt.Errorf("create temporary git credential store: %w", err)
	}
	credentialPath := credentialStore.Name()
	fail := func(err error) (string, error) {
		_ = credentialStore.Close()
		_ = os.Remove(credentialPath)
		return "", err
	}
	if err := credentialStore.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("secure temporary git credential store: %w", err))
	}
	credentialURL := (&url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host, User: url.UserPassword("x-access-token", p.Token)}).String()
	if _, err := credentialStore.WriteString(credentialURL + "\n"); err != nil {
		return fail(fmt.Errorf("write temporary git credential store: %w", err))
	}
	if err := credentialStore.Close(); err != nil {
		_ = os.Remove(credentialPath)
		return "", fmt.Errorf("close temporary git credential store: %w", err)
	}
	return credentialPath, nil
}

func pushCommand(ctx context.Context, binary, workspacePath, credentialStore, lease, pushURL, commit, branch string) *exec.Cmd {
	credentialHelper := "credential.helper=store --file=\"" + filepath.ToSlash(credentialStore) + "\""
	return exec.CommandContext(ctx, binary, "-C", workspacePath, "-c", credentialHelper, "push", "--force-with-lease="+lease, pushURL, commit+":refs/heads/"+branch)
}
