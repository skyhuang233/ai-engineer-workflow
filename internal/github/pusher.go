package github

import (
	"context"
	"errors"
	"fmt"
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
	cmd := exec.CommandContext(ctx, binary, "-C", p.WorkspacePath, "-c", "http.extraHeader=AUTHORIZATION: bearer "+p.Token, "push", "--force-with-lease="+lease, pushURL, commit+":refs/heads/"+branch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push candidate branch: %w (%s)", err, string(output))
	}
	return nil
}
