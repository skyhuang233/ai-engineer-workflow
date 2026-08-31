// Package setup contains the forward-only reconciliation used by `workflow
// setup`. It intentionally has no plan, approval journal, or rollback path:
// each invocation reads the present state and performs the next unmet effect.
package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrRepositoryHistoryConflict = errors.New("Repository History Conflict")
	ErrLocalRepositoryNotFound   = errors.New("local Git repository not found")
)

type RepositoryAddress struct {
	Owner string
	Name  string
}

func (a RepositoryAddress) String() string { return a.Owner + "/" + a.Name }

func (a RepositoryAddress) Validate() error {
	if strings.TrimSpace(a.Owner) == "" || strings.TrimSpace(a.Name) == "" || strings.ContainsAny(a.Owner+a.Name, "\\/?#") {
		return errors.New("invalid GitHub repository address")
	}
	return nil
}

type LocalRepository struct {
	Root      string
	Branch    string
	HasCommit bool
}

func (r LocalRepository) Validate() error {
	if strings.TrimSpace(r.Root) == "" || strings.TrimSpace(r.Branch) == "" {
		return errors.New("resolved repository root and current branch are required")
	}
	return nil
}

type PublicationState int

const (
	PublicationAlreadyPresent PublicationState = iota
	PublicationCanFastForward
	PublicationDiverged
)

// Local is deliberately a small command boundary. Its implementation may use
// Git directly but must never configure, rename, or otherwise mutate remotes.
type Local interface {
	Resolve(context.Context) (LocalRepository, error)
	Initialize(context.Context) (LocalRepository, error)
	CreateEmptyBaseline(context.Context, LocalRepository) (LocalRepository, error)
	PublicationState(context.Context, LocalRepository, RepositoryAddress) (PublicationState, error)
	PublishCurrentBranch(context.Context, LocalRepository, RepositoryAddress) error
}

type GitHubRepository struct {
	Exists        bool
	IssuesEnabled bool
	DefaultBranch string
}

type GitHub interface {
	Repository(context.Context, RepositoryAddress) (GitHubRepository, error)
	CreatePrivateRepository(context.Context, RepositoryAddress) error
	SetDefaultBranch(context.Context, RepositoryAddress, string) error
	EnableIssues(context.Context, RepositoryAddress) error
	LatestIssueID(context.Context, RepositoryAddress) (int64, error)
}

type WatchStore interface {
	RecordWatch(context.Context, string, time.Time, int64) (registeredAt time.Time, inserted bool, err error)
}

type Result struct {
	Root          string
	Repository    string
	Initialized   bool
	BaselineMade  bool
	Created       bool
	Published     bool
	Defaulted     bool
	IssuesEnabled bool
	WatchInserted bool
	RegisteredAt  time.Time
}

// RepositoryReconciler performs only repository-scoped setup effects. The
// caller owns Docker and Worker-authentication readiness and invokes this only
// after those prerequisites have succeeded.
type RepositoryReconciler struct {
	Local   Local
	GitHub  GitHub
	Watches WatchStore
	Now     func() time.Time
}

func (r RepositoryReconciler) Reconcile(ctx context.Context, address RepositoryAddress) (Result, error) {
	if err := address.Validate(); err != nil {
		return Result{}, err
	}
	if r.Local == nil || r.GitHub == nil || r.Watches == nil {
		return Result{}, errors.New("repository setup dependencies are incomplete")
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}

	local, err := r.Local.Resolve(ctx)
	result := Result{Repository: address.String()}
	if errors.Is(err, ErrLocalRepositoryNotFound) {
		local, err = r.Local.Initialize(ctx)
		result.Initialized = err == nil
	}
	if err != nil {
		return result, fmt.Errorf("resolve local repository: %w", err)
	}
	if err := local.Validate(); err != nil {
		return result, err
	}
	result.Root = local.Root
	if !local.HasCommit {
		local, err = r.Local.CreateEmptyBaseline(ctx, local)
		if err != nil {
			return result, fmt.Errorf("create empty baseline: %w", err)
		}
		if err := local.Validate(); err != nil || !local.HasCommit {
			return result, errors.New("empty baseline did not create a current commit")
		}
		result.BaselineMade = true
	}

	remote, err := r.GitHub.Repository(ctx, address)
	if err != nil {
		return result, fmt.Errorf("read GitHub repository: %w", err)
	}
	if !remote.Exists {
		if err := r.GitHub.CreatePrivateRepository(ctx, address); err != nil {
			return result, fmt.Errorf("create GitHub repository: %w", err)
		}
		result.Created = true
		remote, err = r.GitHub.Repository(ctx, address)
		if err != nil || !remote.Exists {
			if err == nil {
				err = errors.New("GitHub repository was absent after creation")
			}
			return result, fmt.Errorf("read created GitHub repository: %w", err)
		}
	}
	publication, err := r.Local.PublicationState(ctx, local, address)
	if err != nil {
		return result, fmt.Errorf("read repository history: %w", err)
	}
	switch publication {
	case PublicationCanFastForward:
		if err := r.Local.PublishCurrentBranch(ctx, local, address); err != nil {
			return result, fmt.Errorf("publish current branch: %w", err)
		}
		result.Published = true
	case PublicationDiverged:
		return result, ErrRepositoryHistoryConflict
	case PublicationAlreadyPresent:
	default:
		return result, errors.New("unknown repository publication state")
	}
	if result.Created && remote.DefaultBranch != local.Branch {
		if err := r.GitHub.SetDefaultBranch(ctx, address, local.Branch); err != nil {
			return result, fmt.Errorf("set initial GitHub default branch: %w", err)
		}
		result.Defaulted = true
	}
	if !remote.IssuesEnabled {
		if err := r.GitHub.EnableIssues(ctx, address); err != nil {
			return result, fmt.Errorf("enable GitHub Issues: %w", err)
		}
		result.IssuesEnabled = true
	}
	boundary, err := r.GitHub.LatestIssueID(ctx, address)
	if err != nil {
		return result, fmt.Errorf("read initial Issue boundary: %w", err)
	}
	if boundary < 0 {
		return result, errors.New("GitHub returned an invalid Issue boundary")
	}
	registered, inserted, err := r.Watches.RecordWatch(ctx, address.String(), now().UTC(), boundary)
	if err != nil {
		return result, fmt.Errorf("record Repository Watch: %w", err)
	}
	result.WatchInserted, result.RegisteredAt = inserted, registered
	return result, nil
}
