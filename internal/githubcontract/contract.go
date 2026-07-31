package githubcontract

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	githubapi "github.com/skyhuang233/workflow/internal/github"
)

const contractBranch = "workflow-credential-contract"

type Verifier struct {
	APIBase string
	Client  *http.Client
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
	var branchCreated bool
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
		if branchCreated {
			if _, err := v.call(cleanupCtx, token, http.MethodDelete, "repos/"+repository+"/git/refs/heads/"+contractBranch, nil, nil); err != nil {
				cleanupErrors = append(cleanupErrors, "delete branch: "+err.Error())
			}
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
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := v.call(ctx, token, http.MethodGet, "repos/"+repository, nil, &repo); err != nil {
		return fmt.Errorf("verify repository metadata: %w", err)
	}
	if _, err := v.call(ctx, token, http.MethodGet, "repos/"+repository+"/actions/workflows", nil, &struct{}{}); err != nil {
		return fmt.Errorf("verify Actions read permission: %w", err)
	}
	if err := v.reconcileStaleArtifacts(ctx, token, owner, repository); err != nil {
		return err
	}
	var base struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := v.call(ctx, token, http.MethodGet, "repos/"+repository+"/git/ref/heads/"+repo.DefaultBranch, nil, &base); err != nil {
		return fmt.Errorf("read integration base ref: %w", err)
	}
	createRef := map[string]string{"ref": "refs/heads/" + contractBranch, "sha": base.Object.SHA}
	status, err := v.call(ctx, token, http.MethodPost, "repos/"+repository+"/git/refs", createRef, &struct{}{})
	if err != nil {
		if status != http.StatusUnprocessableEntity {
			return fmt.Errorf("verify Contents write permission: %w", err)
		}
		if _, deleteErr := v.call(ctx, token, http.MethodDelete, "repos/"+repository+"/git/refs/heads/"+contractBranch, nil, nil); deleteErr != nil {
			return fmt.Errorf("remove stale credential-contract branch: %w", deleteErr)
		}
		if _, createErr := v.call(ctx, token, http.MethodPost, "repos/"+repository+"/git/refs", createRef, &struct{}{}); createErr != nil {
			return fmt.Errorf("recreate credential-contract branch: %w", createErr)
		}
	}
	branchCreated = true

	content := base64.StdEncoding.EncodeToString([]byte("Gateway Credential live contract\n"))
	_, err = v.call(ctx, token, http.MethodPut, "repos/"+repository+"/contents/.workflow-credential-contract",
		map[string]string{"message": "Verify Gateway Credential contract", "content": content, "branch": contractBranch}, &struct{}{})
	if err != nil {
		return fmt.Errorf("verify Contents write permission: %w", err)
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
		map[string]string{"title": "[workflow-contract] Gateway Credential verification", "head": contractBranch, "base": repo.DefaultBranch, "body": "Temporary PR; closed automatically."}, &pull)
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

func (v Verifier) reconcileStaleArtifacts(ctx context.Context, token, owner, repository string) error {
	var pulls []struct {
		Number int `json:"number"`
	}
	pullsPath := "repos/" + repository + "/pulls?state=open&head=" + url.QueryEscape(owner+":"+contractBranch)
	if _, err := v.call(ctx, token, http.MethodGet, pullsPath, nil, &pulls); err != nil {
		return fmt.Errorf("list stale credential-contract PRs: %w", err)
	}
	for _, pull := range pulls {
		if _, err := v.call(ctx, token, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", repository, pull.Number), map[string]string{"state": "closed"}, nil); err != nil {
			return fmt.Errorf("close stale credential-contract PR: %w", err)
		}
	}
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if _, err := v.call(ctx, token, http.MethodGet, "repos/"+repository+"/issues?state=open&labels=workflow-contract", nil, &issues); err != nil {
		return fmt.Errorf("list stale credential-contract issues: %w", err)
	}
	for _, issue := range issues {
		if issue.Title != "[workflow-contract] Gateway Credential verification" {
			continue
		}
		if _, err := v.call(ctx, token, http.MethodPatch, fmt.Sprintf("repos/%s/issues/%d", repository, issue.Number), map[string]string{"state": "closed"}, nil); err != nil {
			return fmt.Errorf("close stale credential-contract issue: %w", err)
		}
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
