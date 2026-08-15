package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/skyhuang233/workflow/internal/credential"
	workflowgithub "github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

// githubCommand is the only GitHub credential bridge exposed to operational
// skills. It reads the admitted repository and owner-bound PAT from Workflow
// Home; the token is never placed in argv, stdout, or durable command output.
func githubCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("workflow github requires an operation")
	}
	operation := args[0]
	flags := flag.NewFlagSet("github "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	source := flags.String("repo", "", "absolute admitted repository checkout")
	home := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	number := flags.Int64("number", 0, "issue or pull request number")
	related := flags.Int64("related", 0, "related issue database id")
	state := flags.String("state", "all", "GitHub state filter")
	label := flags.String("label", "", "label filter or label to add")
	head := flags.String("head", "", "pull request head branch")
	commit := flags.String("commit", "", "commit SHA for check readback")
	title := flags.String("title", "", "issue title")
	bodyFile := flags.String("body-file", "", "UTF-8 body file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if !filepath.IsAbs(*source) {
		return errors.New("workflow github requires an absolute --repo")
	}
	ctx := context.Background()
	layout, err := workflowhome.Resolve(*home)
	if err != nil {
		return err
	}
	database, err := store.OpenReadOnly(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	repository, client, err := managedGitHubClient(ctx, database, layout, *source)
	if err != nil {
		return err
	}
	options := managedGitHubOptions{Number: *number, Related: *related, State: *state, Label: *label, Head: *head, Commit: *commit, Title: *title, BodyFile: *bodyFile}
	if err := executeManagedGitHub(ctx, client, repository, operation, options, output); err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), client.Token, "<redacted>"))
	}
	return nil
}

func managedGitHubClient(ctx context.Context, database *store.Store, layout workflowhome.Layout, source string) (string, *workflowgithub.Client, error) {
	origin, err := readOnlyGitOutput(ctx, source, "remote", "get-url", "origin")
	if err != nil {
		return "", nil, fmt.Errorf("read admitted origin: %w", err)
	}
	repository, err := parseOriginRepository(origin)
	if err != nil {
		return "", nil, err
	}
	admission, err := database.RepositoryAdmission(ctx, repository)
	if err != nil || !admission.Eligible {
		return "", nil, errors.New("repository is not currently admitted")
	}
	verification, err := database.GitHubPATVerification(ctx)
	if err != nil || verification.Status != "verified" {
		return "", nil, errors.New("Control Plane PAT verification is unavailable")
	}
	if !strings.EqualFold(strings.SplitN(repository, "/", 2)[0], verification.Owner) {
		return "", nil, errors.New("admitted repository owner differs from the Workflow Home credential owner")
	}
	if !strings.EqualFold(filepath.Clean(verification.CredentialPath), filepath.Clean(layout.CredentialFile)) {
		return "", nil, errors.New("Control Plane PAT path differs from its Workflow Home binding")
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(ctx, credential.GatewayTarget)
	if err != nil || credential.Fingerprint(token) != verification.FingerprintSHA256 {
		return "", nil, errors.New("Control Plane PAT differs from its verified fingerprint")
	}
	live, err := (githubcredential.Verifier{}).Verify(ctx, token, verification.Owner)
	if err != nil || live.FingerprintSHA256 != verification.FingerprintSHA256 {
		return "", nil, errors.New("Control Plane PAT live verification failed")
	}
	client := workflowgithub.NewClient("", token, nil).WithRepositoryOwner(verification.Owner)
	if err := client.RequireOwnerGuardedRepository(ctx, repository); err != nil {
		return "", nil, err
	}
	return repository, client, nil
}

func readOnlyGitOutput(ctx context.Context, repository string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	raw, err := command.Output()
	return strings.TrimSpace(string(raw)), err
}

type managedGitHubOptions struct {
	Number, Related int64
	State, Label    string
	Head, Title     string
	Commit          string
	BodyFile        string
}

func executeManagedGitHub(ctx context.Context, client *workflowgithub.Client, repository, operation string, options managedGitHubOptions, output io.Writer) error {
	if err := workflowgithub.ValidateOwnerGuardedRepositoryName(repository, client.RepositoryOwner); err != nil {
		return err
	}
	issuePath := func(suffix string) string {
		return "/repos/" + repository + "/issues/" + strconv.FormatInt(options.Number, 10) + suffix
	}
	var result any
	switch operation {
	case "issue-list":
		path := "/repos/" + repository + "/issues?state=" + url.QueryEscape(options.State)
		if options.Label != "" {
			path += "&labels=" + url.QueryEscape(options.Label)
		}
		items, err := paginateManagedGitHub(ctx, client, path)
		if err != nil {
			return err
		}
		result = items
	case "issue-get":
		if options.Number <= 0 {
			return errors.New("issue-get requires --number")
		}
		var item map[string]any
		if err := client.RequestJSON(ctx, http.MethodGet, issuePath(""), nil, &item); err != nil {
			return err
		}
		result = item
	case "issue-create":
		body, err := managedBody(options)
		if err != nil {
			return err
		}
		if options.Title == "" || options.Label == "" {
			return errors.New("issue-create requires --title, --body-file, and --label")
		}
		var item map[string]any
		if err := client.RequestJSON(ctx, http.MethodPost, "/repos/"+repository+"/issues", map[string]any{"title": options.Title, "body": body, "labels": []string{options.Label}}, &item); err != nil {
			return err
		}
		result = item
	case "issue-label":
		if options.Number <= 0 || options.Label == "" {
			return errors.New("issue-label requires --number and --label")
		}
		if err := client.RequestJSON(ctx, http.MethodPost, issuePath("/labels"), map[string]any{"labels": []string{options.Label}}, &result); err != nil {
			return err
		}
	case "issue-comments", "pr-comments", "pr-reviews":
		if options.Number <= 0 {
			return errors.New(operation + " requires --number")
		}
		path := issuePath("/comments")
		if operation == "pr-comments" {
			path = "/repos/" + repository + "/pulls/" + strconv.FormatInt(options.Number, 10) + "/comments"
		}
		if operation == "pr-reviews" {
			path = "/repos/" + repository + "/pulls/" + strconv.FormatInt(options.Number, 10) + "/reviews"
		}
		items, err := paginateManagedGitHub(ctx, client, path)
		if err != nil {
			return err
		}
		result = items
	case "subissues-list", "blocked-by-list":
		if options.Number <= 0 {
			return errors.New(operation + " requires --number")
		}
		suffix := "/sub_issues"
		if operation == "blocked-by-list" {
			suffix = "/dependencies/blocked_by"
		}
		items, err := paginateManagedGitHub(ctx, client, issuePath(suffix))
		if err != nil {
			return err
		}
		result = items
	case "subissues-add", "blocked-by-add":
		if options.Number <= 0 || options.Related <= 0 {
			return errors.New(operation + " requires --number and --related database id")
		}
		suffix, key := "/sub_issues", "sub_issue_id"
		if operation == "blocked-by-add" {
			suffix, key = "/dependencies/blocked_by", "issue_id"
		}
		if err := client.RequestJSON(ctx, http.MethodPost, issuePath(suffix), map[string]int64{key: options.Related}, &result); err != nil {
			return err
		}
	case "pr-list":
		path := "/repos/" + repository + "/pulls?state=" + url.QueryEscape(options.State)
		if options.Head != "" {
			path += "&head=" + url.QueryEscape(options.Head)
		}
		items, err := paginateManagedGitHub(ctx, client, path)
		if err != nil {
			return err
		}
		result = items
	case "pr-get":
		if options.Number <= 0 {
			return errors.New("pr-get requires --number")
		}
		if err := client.RequestJSON(ctx, http.MethodGet, "/repos/"+repository+"/pulls/"+strconv.FormatInt(options.Number, 10), nil, &result); err != nil {
			return err
		}
	case "commit-checks":
		if strings.TrimSpace(options.Commit) == "" {
			return errors.New("commit-checks requires --commit")
		}
		checks, err := paginateManagedCheckRuns(ctx, client, "/repos/"+repository+"/commits/"+url.PathEscape(options.Commit)+"/check-runs?filter=latest")
		if err != nil {
			return err
		}
		result = checks
	default:
		return fmt.Errorf("unsupported managed GitHub operation %q", operation)
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}

func paginateManagedCheckRuns(ctx context.Context, client *workflowgithub.Client, path string) ([]map[string]any, error) {
	all := []map[string]any{}
	for page := 1; ; page++ {
		var response struct {
			CheckRuns []map[string]any `json:"check_runs"`
		}
		if err := client.RequestJSON(ctx, http.MethodGet, path+"&per_page=100&page="+strconv.Itoa(page), nil, &response); err != nil {
			return nil, err
		}
		all = append(all, response.CheckRuns...)
		if len(response.CheckRuns) < 100 {
			return all, nil
		}
	}
}

func managedBody(options managedGitHubOptions) (string, error) {
	if strings.TrimSpace(options.BodyFile) == "" {
		return "", errors.New("--body-file is required")
	}
	raw, err := os.ReadFile(options.BodyFile)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func paginateManagedGitHub(ctx context.Context, client *workflowgithub.Client, path string) ([]map[string]any, error) {
	all := []map[string]any{}
	for page := 1; ; page++ {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		var batch []map[string]any
		if err := client.RequestJSON(ctx, http.MethodGet, path+separator+"per_page=100&page="+strconv.Itoa(page), nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}
