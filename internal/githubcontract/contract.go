package githubcontract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	githubapi "github.com/skyhuang233/workflow/internal/github"
)

const contractBranchPrefix = "workflow-credential-contract-"

type GitPushArtifact struct {
	Branch  string
	Commit  string
	cleanup func()
}

type Verifier struct {
	APIBase string
	Client  *http.Client
	Push    func(context.Context, string, string, string) (GitPushArtifact, error)
}

func (v Verifier) Verify(ctx context.Context, token, owner, repository string) (resultErr error) {
	if !strings.HasPrefix(strings.TrimSpace(token), "github_pat_") {
		return errors.New("a fine-grained PAT is required")
	}
	if v.APIBase == "" {
		v.APIBase = "https://api.github.com"
	}
	if v.Client == nil {
		v.Client = http.DefaultClient
	}
	var gitArtifact GitPushArtifact
	var issueNumber, pullNumber int
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cleanupErrors []string
		if pullNumber != 0 {
			if _, err := v.call(cleanupCtx, token, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", repository, pullNumber), map[string]string{"state": "closed"}, nil); err != nil {
				cleanupErrors = append(cleanupErrors, "close PR: "+err.Error())
			}
		}
		if issueNumber != 0 {
			if _, err := v.call(cleanupCtx, token, http.MethodPatch, fmt.Sprintf("repos/%s/issues/%d", repository, issueNumber), map[string]string{"state": "closed"}, nil); err != nil {
				cleanupErrors = append(cleanupErrors, "close issue: "+err.Error())
			}
		}
		if gitArtifact.Branch != "" {
			if err := v.cleanupGitArtifact(cleanupCtx, token, repository, gitArtifact); err != nil {
				cleanupErrors = append(cleanupErrors, "delete branch: "+err.Error())
			}
		}
		if gitArtifact.cleanup != nil {
			gitArtifact.cleanup()
		}
		if len(cleanupErrors) > 0 {
			resultErr = errors.Join(resultErr, errors.New("credential contract cleanup failed: "+strings.Join(cleanupErrors, "; ")))
		}
	}()
	var identity struct {
		Login string `json:"login"`
	}
	if _, err := v.call(ctx, token, http.MethodGet, "user", nil, &identity); err != nil {
		return fmt.Errorf("verify metadata permission: %w", err)
	}
	if identity.Login != owner {
		return fmt.Errorf("credential owner %q does not match %q", identity.Login, owner)
	}
	if err := githubapi.ValidateOwnerGuardedRepositoryName(repository, owner); err != nil {
		return err
	}
	var repo githubapi.RepositoryMetadata
	if _, err := v.call(ctx, token, http.MethodGet, "repos/"+repository, nil, &repo); err != nil {
		return fmt.Errorf("verify repository metadata: %w", err)
	}
	if err := repo.ValidateOwnerGuarded(repository, owner); err != nil {
		return err
	}
	if _, err := v.call(ctx, token, http.MethodGet, "repos/"+repository+"/actions/workflows", nil, &struct{}{}); err != nil {
		return fmt.Errorf("verify Actions read permission: %w", err)
	}
	push := v.Push
	if push == nil {
		push = verifyGitPush
	}
	artifact, err := push(ctx, token, repository, repo.DefaultBranch)
	if err != nil {
		return fmt.Errorf("verify Git HTTPS push permission: %w", err)
	}
	gitArtifact = artifact
	if !validGitPushArtifact(gitArtifact) {
		return errors.New("Git HTTPS push verification returned an invalid temporary artifact")
	}
	var issue struct {
		Number int `json:"number"`
	}
	_, err = v.call(ctx, token, http.MethodPost, "repos/"+repository+"/issues",
		map[string]any{"title": "[workflow-contract] Gateway Credential verification", "body": "Temporary issue; closed automatically."}, &issue)
	if err != nil {
		return fmt.Errorf("verify Issues write permission: %w", err)
	}
	issueNumber = issue.Number
	label := map[string]string{"name": "workflow-contract", "color": "0969da", "description": "Temporary workflow integration contract"}
	status, labelErr := v.call(ctx, token, http.MethodPost, "repos/"+repository+"/labels", label, &struct{}{})
	if labelErr != nil && status != http.StatusUnprocessableEntity {
		return fmt.Errorf("verify label update permission: %w", labelErr)
	}
	if _, err := v.call(ctx, token, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/labels", repository, issue.Number),
		map[string][]string{"labels": {"workflow-contract"}}, &struct{}{}); err != nil {
		return fmt.Errorf("verify label application permission: %w", err)
	}

	var pull struct {
		Number int `json:"number"`
	}
	_, err = v.call(ctx, token, http.MethodPost, "repos/"+repository+"/pulls",
		map[string]string{"title": "[workflow-contract] Gateway Credential verification", "head": gitArtifact.Branch, "base": repo.DefaultBranch, "body": "Temporary PR; closed automatically."}, &pull)
	if err != nil {
		return fmt.Errorf("verify Pull requests write permission: %w", err)
	}
	pullNumber = pull.Number
	if _, err := v.call(ctx, token, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", repository, pull.Number),
		map[string]string{"body": "Gateway Credential write contract verified."}, &struct{}{}); err != nil {
		return fmt.Errorf("verify issue comment permission: %w", err)
	}
	return nil
}

func verifyGitPush(ctx context.Context, token, repository, defaultBranch string) (artifact GitPushArtifact, resultErr error) {
	if defaultBranch == "" {
		return GitPushArtifact{}, errors.New("repository default branch is required")
	}
	entropy := make([]byte, 12)
	if _, err := rand.Read(entropy); err != nil {
		return GitPushArtifact{}, fmt.Errorf("generate contract branch suffix: %w", err)
	}
	branch := contractBranchPrefix + hex.EncodeToString(entropy)
	workspace, err := os.MkdirTemp("", "workflow-credential-contract-")
	if err != nil {
		return GitPushArtifact{}, fmt.Errorf("create contract workspace: %w", err)
	}
	removeWorkspace := func() {
		if removeErr := os.RemoveAll(workspace); removeErr != nil && resultErr == nil {
			resultErr = fmt.Errorf("remove contract workspace: %w", removeErr)
		}
	}
	defer func() {
		if resultErr != nil {
			removeWorkspace()
		}
	}()
	pushURL := "https://github.com/" + repository + ".git"
	repositoryStore, err := git.PlainCloneContext(ctx, workspace, false, &git.CloneOptions{
		URL:           pushURL,
		Auth:          &githttp.BasicAuth{Username: "x-access-token", Password: token},
		ReferenceName: plumbing.NewBranchReferenceName(defaultBranch),
		SingleBranch:  true,
	})
	if err != nil {
		return GitPushArtifact{}, fmt.Errorf("clone integration repository: %w", err)
	}
	marker := ".workflow-credential-contract"
	if err := os.WriteFile(filepath.Join(workspace, marker), []byte("Gateway Credential Git push contract\n"), 0o600); err != nil {
		return GitPushArtifact{}, fmt.Errorf("write contract marker: %w", err)
	}
	worktree, err := repositoryStore.Worktree()
	if err != nil {
		return GitPushArtifact{}, fmt.Errorf("open contract worktree: %w", err)
	}
	if _, err := worktree.Add(marker); err != nil {
		return GitPushArtifact{}, fmt.Errorf("stage contract marker: %w", err)
	}
	commit, err := worktree.Commit("Verify Gateway Credential contract", &git.CommitOptions{Author: &object.Signature{Name: "workflow credential contract", Email: "workflow-contract@localhost", When: time.Now().UTC()}})
	if err != nil {
		return GitPushArtifact{}, fmt.Errorf("commit contract marker: %w", err)
	}
	pusher := githubapi.WorkspacePusher{WorkspacePath: workspace, Token: token, PushURL: pushURL}
	if err := pusher.Push(ctx, repository, branch, commit.String(), "", true); err != nil {
		return GitPushArtifact{}, err
	}
	return GitPushArtifact{Branch: branch, Commit: commit.String(), cleanup: removeWorkspace}, nil
}

func validGitPushArtifact(artifact GitPushArtifact) bool {
	if !strings.HasPrefix(artifact.Branch, contractBranchPrefix) || len(artifact.Branch) != len(contractBranchPrefix)+24 || len(artifact.Commit) != 40 {
		return false
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(artifact.Branch, contractBranchPrefix)); err != nil {
		return false
	}
	_, err := hex.DecodeString(artifact.Commit)
	return err == nil
}

func (v Verifier) cleanupGitArtifact(ctx context.Context, token, repository string, artifact GitPushArtifact) error {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := v.call(ctx, token, http.MethodGet, "repos/"+repository+"/git/ref/heads/"+artifact.Branch, nil, &ref)
	if err != nil {
		if status == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("verify temporary branch ownership: %w", err)
	}
	if !strings.EqualFold(ref.Object.SHA, artifact.Commit) {
		return errors.New("temporary branch head changed; refusing cleanup")
	}
	if _, err := v.call(ctx, token, http.MethodDelete, "repos/"+repository+"/git/refs/heads/"+artifact.Branch, nil, nil); err != nil {
		return fmt.Errorf("delete verified temporary branch: %w", err)
	}
	return nil
}

func (v Verifier) call(ctx context.Context, token, method, path string, body, destination any) (int, error) {
	client := githubapi.NewClient(v.APIBase, strings.TrimSpace(token), v.Client)
	err := client.RequestJSON(ctx, method, "/"+path, body, destination)
	if err == nil {
		return http.StatusOK, nil
	}
	var apiErr *githubapi.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, err
	}
	return 0, err
}
