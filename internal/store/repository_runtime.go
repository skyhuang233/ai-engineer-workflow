package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var repositoryIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

var ErrRepositoryRuntimeNotConfigured = errors.New("repository runtime is not configured")

// RepositoryRuntimeConfiguration is the durable, repository-scoped input to
// the legacy polling/reconciliation/scheduling loop. Session-only Gateway
// addresses and credentials are deliberately derived by the Control Plane.
type RepositoryRuntimeConfiguration struct {
	Repository      string
	DefaultBranch   string
	SourcePath      string
	RootIssueNumber int64
	WorkspaceRoot   string
	StateRoot       string
	// CodexAuthFile is retained only for source compatibility with pre-mode
	// callers. It is no longer persisted or required: execution authentication
	// is a host-scoped selection owned by internal/executionauth.
	CodexAuthFile      string
	GitHubAPIURL       string
	PollInterval       time.Duration
	WorkspaceRetention time.Duration
	MaxParallelRuns    int
	UpdatedAt          time.Time
}

func (c RepositoryRuntimeConfiguration) Validate() error {
	if !repositoryIdentityPattern.MatchString(c.Repository) || strings.TrimSpace(c.DefaultBranch) == "" {
		return errors.New("repository runtime identity and default branch are required")
	}
	if c.RootIssueNumber < 0 {
		return errors.New("repository runtime root issue number must not be negative")
	}
	for name, value := range map[string]string{"source path": c.SourcePath, "workspace root": c.WorkspaceRoot, "state root": c.StateRoot} {
		if value != "" && !filepath.IsAbs(value) {
			return fmt.Errorf("repository runtime %s must be absolute", name)
		}
	}
	parsed, err := url.Parse(c.GitHubAPIURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("repository runtime GitHub API URL must be absolute HTTPS")
	}
	if c.PollInterval <= 0 || c.WorkspaceRetention <= 0 || c.MaxParallelRuns <= 0 || c.UpdatedAt.IsZero() {
		return errors.New("repository runtime timing, parallelism, and update time must be positive")
	}
	return nil
}

func (c RepositoryRuntimeConfiguration) Ready() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.RootIssueNumber == 0 || c.SourcePath == "" || c.WorkspaceRoot == "" || c.StateRoot == "" {
		return ErrRepositoryRuntimeNotConfigured
	}
	return nil
}

const (
	repositoryRuntimePreserveConflict = ` ON CONFLICT(repository) DO NOTHING`
	repositoryRuntimeReplaceConflict  = ` ON CONFLICT(repository) DO UPDATE SET default_branch=excluded.default_branch,source_path=excluded.source_path,root_issue_number=excluded.root_issue_number,workspace_root=excluded.workspace_root,state_root=excluded.state_root,github_api_url=excluded.github_api_url,poll_interval_seconds=excluded.poll_interval_seconds,workspace_retention_seconds=excluded.workspace_retention_seconds,max_parallel_runs=excluded.max_parallel_runs,updated_at=excluded.updated_at`
)

func recordRepositoryRuntimeConfiguration(ctx context.Context, executor repositoryRecordExecutor, value RepositoryRuntimeConfiguration, conflictAction string) error {
	if err := value.Validate(); err != nil {
		return err
	}
	cleanOptionalPath := func(path string) string {
		if path == "" {
			return ""
		}
		return filepath.Clean(path)
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO repository_runtime_configurations(repository,default_branch,source_path,root_issue_number,workspace_root,state_root,github_api_url,poll_interval_seconds,workspace_retention_seconds,max_parallel_runs,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`+conflictAction,
		value.Repository, value.DefaultBranch, cleanOptionalPath(value.SourcePath), value.RootIssueNumber, cleanOptionalPath(value.WorkspaceRoot), cleanOptionalPath(value.StateRoot), value.GitHubAPIURL, int64(value.PollInterval/time.Second), int64(value.WorkspaceRetention/time.Second), value.MaxParallelRuns, formatTimestamp(value.UpdatedAt))
	return err
}

func (s *Store) RecordRepositoryRuntimeConfiguration(ctx context.Context, value RepositoryRuntimeConfiguration) error {
	return recordRepositoryRuntimeConfiguration(ctx, s.db, value, repositoryRuntimeReplaceConflict)
}

func (s *Store) RepositoryRuntimeConfiguration(ctx context.Context, repository string) (RepositoryRuntimeConfiguration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repository,default_branch,source_path,root_issue_number,workspace_root,state_root,github_api_url,poll_interval_seconds,workspace_retention_seconds,max_parallel_runs,updated_at FROM repository_runtime_configurations WHERE repository=?`, repository)
	value, err := scanRepositoryRuntime(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryRuntimeConfiguration{}, ErrNotFound
	}
	return value, err
}

func (s *Store) RepositoryRuntimeConfigurations(ctx context.Context) ([]RepositoryRuntimeConfiguration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository,default_branch,source_path,root_issue_number,workspace_root,state_root,github_api_url,poll_interval_seconds,workspace_retention_seconds,max_parallel_runs,updated_at FROM repository_runtime_configurations ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []RepositoryRuntimeConfiguration
	for rows.Next() {
		value, scanErr := scanRepositoryRuntime(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

type scanValues func(...any) error

func scanRepositoryRuntime(scan scanValues) (RepositoryRuntimeConfiguration, error) {
	var value RepositoryRuntimeConfiguration
	var pollSeconds, retentionSeconds int64
	var updated string
	err := scan(&value.Repository, &value.DefaultBranch, &value.SourcePath, &value.RootIssueNumber, &value.WorkspaceRoot, &value.StateRoot, &value.GitHubAPIURL, &pollSeconds, &retentionSeconds, &value.MaxParallelRuns, &updated)
	if err != nil {
		return value, err
	}
	value.PollInterval = time.Duration(pollSeconds) * time.Second
	value.WorkspaceRetention = time.Duration(retentionSeconds) * time.Second
	value.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return value, err
}
