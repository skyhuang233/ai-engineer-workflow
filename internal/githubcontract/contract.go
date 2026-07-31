package githubcontract

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const contractBranch = "workflow-credential-contract"

type Verifier struct {
	APIBase string
	Client  *http.Client
}

func (v Verifier) Verify(ctx context.Context, token, owner, repository string) error {
	if !strings.HasPrefix(strings.TrimSpace(token), "github_pat_") {
		return errors.New("a fine-grained PAT is required")
	}
	if v.APIBase == "" {
		v.APIBase = "https://api.github.com"
	}
	if v.Client == nil {
		v.Client = http.DefaultClient
	}
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
	defer v.call(ctx, token, http.MethodDelete, "repos/"+repository+"/git/refs/heads/"+contractBranch, nil, nil)

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
	defer v.call(ctx, token, http.MethodPatch, fmt.Sprintf("repos/%s/issues/%d", repository, issue.Number), map[string]string{"state": "closed"}, nil)

	var pull struct {
		Number int `json:"number"`
	}
	_, err = v.call(ctx, token, http.MethodPost, "repos/"+repository+"/pulls",
		map[string]string{"title": "[workflow-contract] Gateway Credential verification", "head": contractBranch, "base": repo.DefaultBranch, "body": "Temporary PR; closed automatically."}, &pull)
	if err != nil {
		return fmt.Errorf("verify Pull requests write permission: %w", err)
	}
	defer v.call(ctx, token, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", repository, pull.Number), map[string]string{"state": "closed"}, nil)
	if _, err := v.call(ctx, token, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", repository, pull.Number),
		map[string]string{"body": "Gateway Credential write contract verified."}, &struct{}{}); err != nil {
		return fmt.Errorf("verify issue comment permission: %w", err)
	}
	return nil
}

func (v Verifier) call(ctx context.Context, token, method, path string, body, destination any) (int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.APIBase, "/")+"/"+path, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := v.Client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return response.StatusCode, fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	if destination == nil || response.StatusCode == http.StatusNoContent {
		return response.StatusCode, nil
	}
	return response.StatusCode, json.NewDecoder(response.Body).Decode(destination)
}
