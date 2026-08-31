package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/hostsetup"
	setupjourney "github.com/skyhuang233/workflow/internal/setup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

// setupCommand owns direct, forward repository reconciliation. Authentication
// and Worker-runtime preparation are deliberately separate capabilities, so
// this command does not accept credentials or write repository configuration.
func setupCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	home := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	databasePath := flags.String("database", "", "advanced absolute SQLite Watch Store override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("workflow setup accepts flags only")
	}
	layout, err := workflowhome.Resolve(*home)
	if err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	if *databasePath == "" {
		*databasePath = filepath.Join(layout.State, "workflow.db")
	}
	if !filepath.IsAbs(*databasePath) {
		return errors.New("--database override must be absolute")
	}
	database, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	workerRelease, err := database.ActiveWorkerRelease(context.Background())
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("Worker runtime is not prepared; complete Platform Preparation before repository reconciliation")
	}
	if err != nil {
		return err
	}
	if _, err := (setupjourney.PlatformPreparer{
		WorkerImage: workerRelease.ImageReference, StateRoot: layout.State, WorkspaceRoot: layout.Workspaces,
		Probe: dockerWorkerProbe{}, Authentication: codexLoginAuthentication{},
	}).Prepare(context.Background()); err != nil {
		return err
	}

	local := gitSetupLocal{Directory: currentWorkingDirectory}
	resolved, err := local.Resolve(context.Background())
	if err != nil && !errors.Is(err, setupjourney.ErrLocalRepositoryNotFound) {
		return err
	}
	address, err := resolveSetupAddress(context.Background(), local, resolved)
	if err != nil {
		return err
	}
	reconciler := setupjourney.RepositoryReconciler{
		Local: local, GitHub: ghSetupGitHub{}, Watches: storeWatchAdapter{store: database},
	}
	result, err := reconciler.Reconcile(context.Background(), address)
	if err != nil {
		return err
	}
	watch, err := database.RepositoryWatch(context.Background(), result.Repository)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	native, err := controlplane.NewNativeService(controlplane.NativeServiceOptions{Executable: executable, WorkflowHome: layout.Root})
	if err != nil {
		return err
	}
	activation, err := setupjourney.Activate(context.Background(), native, database, watch, result.WatchInserted, 2*time.Minute, time.Now)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"status": "ready", "repository": result, "activation": activation})
}

var currentWorkingDirectory = os.Getwd

type commandResultError struct{ output string }

func (e commandResultError) Error() string { return strings.TrimSpace(e.output) }

func runSetupCommand(ctx context.Context, directory string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return output, commandResultError{output: string(output)}
	}
	return output, nil
}

type gitSetupLocal struct{ Directory func() (string, error) }

func (g gitSetupLocal) directory() (string, error) {
	if g.Directory == nil {
		return "", errors.New("current directory resolver is required")
	}
	return g.Directory()
}
func (g gitSetupLocal) Resolve(ctx context.Context) (setupjourney.LocalRepository, error) {
	directory, err := g.directory()
	if err != nil {
		return setupjourney.LocalRepository{}, err
	}
	root, err := runSetupCommand(ctx, directory, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return setupjourney.LocalRepository{}, setupjourney.ErrLocalRepositoryNotFound
	}
	rootPath := strings.TrimSpace(string(root))
	branch, err := runSetupCommand(ctx, rootPath, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return setupjourney.LocalRepository{}, errors.New("detached HEAD blocks repository setup")
	}
	_, headErr := runSetupCommand(ctx, rootPath, "git", "rev-parse", "--verify", "HEAD")
	return setupjourney.LocalRepository{Root: rootPath, Branch: strings.TrimSpace(string(branch)), HasCommit: headErr == nil}, nil
}
func (g gitSetupLocal) Initialize(ctx context.Context) (setupjourney.LocalRepository, error) {
	directory, err := g.directory()
	if err != nil {
		return setupjourney.LocalRepository{}, err
	}
	if output, err := runSetupCommand(ctx, directory, "git", "init"); err != nil {
		return setupjourney.LocalRepository{}, fmt.Errorf("git init: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return g.Resolve(ctx)
}
func (g gitSetupLocal) CreateEmptyBaseline(ctx context.Context, local setupjourney.LocalRepository) (setupjourney.LocalRepository, error) {
	if output, err := runSetupCommand(ctx, local.Root, "git", "commit", "--allow-empty", "-m", "Initial commit"); err != nil {
		return setupjourney.LocalRepository{}, fmt.Errorf("git commit --allow-empty: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return g.Resolve(ctx)
}
func (g gitSetupLocal) PublicationState(ctx context.Context, local setupjourney.LocalRepository, address setupjourney.RepositoryAddress) (setupjourney.PublicationState, error) {
	remote := githubGitURL(address)
	head, err := runSetupCommand(ctx, local.Root, "git", "rev-parse", "HEAD")
	if err != nil {
		return 0, err
	}
	remoteHead, err := runSetupCommand(ctx, local.Root, "git", "ls-remote", remote, "refs/heads/"+local.Branch)
	if err != nil {
		return 0, fmt.Errorf("git ls-remote: %w (%s)", err, strings.TrimSpace(string(remoteHead)))
	}
	fields := strings.Fields(string(remoteHead))
	if len(fields) == 0 {
		return setupjourney.PublicationCanFastForward, nil
	}
	if strings.EqualFold(fields[0], strings.TrimSpace(string(head))) {
		return setupjourney.PublicationAlreadyPresent, nil
	}
	dryRun, err := runSetupCommand(ctx, local.Root, "git", "push", "--dry-run", remote, "HEAD:refs/heads/"+local.Branch)
	if err == nil {
		return setupjourney.PublicationCanFastForward, nil
	}
	if strings.Contains(strings.ToLower(string(dryRun)), "non-fast-forward") || strings.Contains(strings.ToLower(string(dryRun)), "fetch first") {
		return setupjourney.PublicationDiverged, nil
	}
	return 0, fmt.Errorf("check publication fast-forward: %w (%s)", err, strings.TrimSpace(string(dryRun)))
}
func (g gitSetupLocal) PublishCurrentBranch(ctx context.Context, local setupjourney.LocalRepository, address setupjourney.RepositoryAddress) error {
	output, err := runSetupCommand(ctx, local.Root, "git", "push", githubGitURL(address), "HEAD:refs/heads/"+local.Branch)
	if err != nil {
		return fmt.Errorf("git push: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func githubGitURL(address setupjourney.RepositoryAddress) string {
	return "https://github.com/" + address.String() + ".git"
}

func resolveSetupAddress(ctx context.Context, local gitSetupLocal, resolved setupjourney.LocalRepository) (setupjourney.RepositoryAddress, error) {
	if resolved.Root != "" {
		if origin, err := runSetupCommand(ctx, resolved.Root, "git", "remote", "get-url", "origin"); err == nil {
			if address, ok := parseGitHubRemote(strings.TrimSpace(string(origin))); ok {
				return address, nil
			}
		}
	}
	directory, err := local.directory()
	if err != nil {
		return setupjourney.RepositoryAddress{}, err
	}
	login, err := runSetupCommand(ctx, directory, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return setupjourney.RepositoryAddress{}, fmt.Errorf("active GitHub CLI login is required: %w", err)
	}
	name := filepath.Base(directory)
	if resolved.Root != "" {
		name = filepath.Base(resolved.Root)
	}
	address := setupjourney.RepositoryAddress{Owner: strings.TrimSpace(string(login)), Name: name}
	return address, address.Validate()
}

func parseGitHubRemote(remote string) (setupjourney.RepositoryAddress, bool) {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	remote = strings.TrimPrefix(remote, "git@github.com:")
	remote = strings.TrimPrefix(remote, "ssh://git@github.com/")
	remote = strings.TrimPrefix(remote, "https://github.com/")
	remote = strings.TrimPrefix(remote, "http://github.com/")
	parts := strings.Split(remote, "/")
	if len(parts) != 2 {
		return setupjourney.RepositoryAddress{}, false
	}
	address := setupjourney.RepositoryAddress{Owner: parts[0], Name: parts[1]}
	return address, address.Validate() == nil
}

type ghSetupGitHub struct{}
type ghRepositoryJSON struct {
	FullName  string `json:"full_name"`
	HasIssues bool   `json:"has_issues"`
}

func (ghSetupGitHub) Repository(ctx context.Context, address setupjourney.RepositoryAddress) (setupjourney.GitHubRepository, error) {
	output, err := runSetupCommand(ctx, "", "gh", "api", "repos/"+address.String())
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "404") {
		return setupjourney.GitHubRepository{}, nil
	}
	if err != nil {
		return setupjourney.GitHubRepository{}, err
	}
	var value ghRepositoryJSON
	if err := json.Unmarshal(output, &value); err != nil {
		return setupjourney.GitHubRepository{}, err
	}
	if !strings.EqualFold(value.FullName, address.String()) {
		return setupjourney.GitHubRepository{}, errors.New("GitHub returned a different repository")
	}
	return setupjourney.GitHubRepository{Exists: true, IssuesEnabled: value.HasIssues}, nil
}
func (ghSetupGitHub) CreatePrivateRepository(ctx context.Context, address setupjourney.RepositoryAddress) error {
	endpoint := "user/repos"
	account, err := runSetupCommand(ctx, "", "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(string(account)), address.Owner) {
		endpoint = "orgs/" + address.Owner + "/repos"
	}
	output, err := runSetupCommand(ctx, "", "gh", "api", "--method", "POST", endpoint, "-f", "name="+address.Name, "-F", "private=true")
	if err != nil {
		return fmt.Errorf("gh api create repository: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
func (ghSetupGitHub) EnableIssues(ctx context.Context, address setupjourney.RepositoryAddress) error {
	output, err := runSetupCommand(ctx, "", "gh", "api", "--method", "PATCH", "repos/"+address.String(), "-F", "has_issues=true")
	if err != nil {
		return fmt.Errorf("gh api enable Issues: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
func (ghSetupGitHub) LatestIssueID(ctx context.Context, address setupjourney.RepositoryAddress) (int64, error) {
	output, err := runSetupCommand(ctx, "", "gh", "api", "repos/"+address.String()+"/issues?state=all&sort=created&direction=desc&per_page=1", "--jq", ".[0].id // 0")
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode Issue boundary: %w", err)
	}
	return value, nil
}

type storeWatchAdapter struct{ store *store.Store }

func (a storeWatchAdapter) RecordWatch(ctx context.Context, repository string, registered time.Time, cursor int64) (time.Time, bool, error) {
	watch, inserted, err := a.store.RecordRepositoryWatch(ctx, store.RepositoryWatch{Repository: repository, RegisteredAt: registered, IssueCursor: cursor})
	return watch.RegisteredAt, inserted, err
}

type dockerWorkerProbe struct{}

func (dockerWorkerProbe) Verify(ctx context.Context, image, stateRoot, workspaceRoot string) error {
	return hostsetup.VerifyDockerWorker(ctx, nil, image, stateRoot, workspaceRoot)
}

type codexLoginAuthentication struct{}

func (codexLoginAuthentication) Ready(ctx context.Context) (string, bool, error) {
	if _, err := codexauth.ResolveChatGPT(ctx); err != nil {
		return "API key or Codex login", false, nil
	}
	return "Codex login", true, nil
}
