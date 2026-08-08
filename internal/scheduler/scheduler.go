package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type Dispatcher struct {
	Store            *store.Store
	Reader           plan.SnapshotReader
	Projector        plan.RootProjector
	MaxParallelRuns  int
	LeaseTTL         time.Duration
	Now              func() time.Time
	Recovery         RecoveryInspector
	HostPressure     HostPressureInspector
	ProvisionSession store.SessionProvisioner
}

type RecoveryInspector interface {
	ContainerRunning(context.Context, string) (bool, error)
	IsolateContainer(context.Context, string) error
	WorkspaceAvailable(context.Context, store.TicketSession) (bool, error)
}

type HostPressureInspector interface {
	Inspect(context.Context) (string, error)
}

// HostPressure explains a dispatch-only safety gate. It never authorizes
// ending an active Worker Run.
type HostPressure struct {
	Reason string
}

func (p HostPressure) Unsafe() bool { return p.Reason != "" }

// Claim reads the Plan Root only for the current human body and repository
// identity. Store.ClaimReady remains the atomic authority; projection failure
// leaves the durable claim intact for the next reconcile pass.
func (d Dispatcher) Claim(ctx context.Context, repository string, rootNumber, ticketID int64, owner string) (store.TicketClaim, error) {
	if d.Store == nil || d.Reader == nil || d.Projector == nil {
		return store.TicketClaim{}, fmt.Errorf("scheduler dependencies are incomplete")
	}
	snapshot, err := d.Reader.ReadPlan(ctx, repository, rootNumber)
	if err != nil {
		return store.TicketClaim{}, err
	}
	version, err := d.Store.CurrentVersion(ctx, repository, snapshot.Root.ID)
	if err != nil {
		return store.TicketClaim{}, err
	}
	if pressure, err := d.hostPressure(ctx); err != nil {
		return store.TicketClaim{}, err
	} else if pressure.Unsafe() {
		if err := d.project(ctx, repository, rootNumber, version.ID, pressure.Reason); err != nil {
			return store.TicketClaim{}, err
		}
		return store.TicketClaim{}, store.ErrCapacity
	}
	now := d.now()
	claim, err := d.Store.ClaimReady(ctx, store.ClaimRequest{
		VersionID:        version.ID,
		TicketID:         ticketID,
		Owner:            owner,
		MaxParallelRuns:  d.MaxParallelRuns,
		LeaseTTL:         d.LeaseTTL,
		Now:              now,
		ProvisionSession: d.ProvisionSession,
	})
	if err != nil {
		return store.TicketClaim{}, err
	}
	if err := d.project(ctx, repository, rootNumber, version.ID, ""); err != nil {
		return claim, fmt.Errorf("project ticket claim: %w", err)
	}
	return claim, nil
}

// ClaimNext dispatches from every active Delivery Plan under one global
// capacity and durable fairness cursor. Only the selected Plan Root is
// projected after a successful claim.
func (d Dispatcher) ClaimNext(ctx context.Context, repository, owner string) (store.TicketClaim, error) {
	if d.Store == nil || d.Projector == nil {
		return store.TicketClaim{}, fmt.Errorf("scheduler dependencies are incomplete")
	}
	paused, err := d.DispatchPaused(ctx, repository)
	if err != nil {
		return store.TicketClaim{}, err
	}
	if paused {
		return store.TicketClaim{}, store.ErrCapacity
	}
	now := d.now()
	claim, err := d.Store.ClaimNextReady(ctx, repository, owner, d.MaxParallelRuns, 0, d.LeaseTTL, now, d.ProvisionSession)
	if err != nil {
		return store.TicketClaim{}, err
	}
	if err := d.project(ctx, repository, claim.PlanRootNumber, claim.VersionID, ""); err != nil {
		return claim, fmt.Errorf("project ticket claim: %w", err)
	}
	return claim, nil
}

// DispatchPaused gates every new Worker Run, including Delivery Controller
// retries. It records an explanatory projection before the caller can return
// under pressure and leaves active Runs untouched.
func (d Dispatcher) DispatchPaused(ctx context.Context, repository string) (bool, error) {
	if d.Store == nil || d.Projector == nil {
		return false, fmt.Errorf("scheduler dependencies are incomplete")
	}
	pressure, err := d.hostPressure(ctx)
	if err != nil {
		return false, err
	}
	if !pressure.Unsafe() {
		return false, nil
	}
	roots, err := d.Store.ActivePlanRoots(ctx, repository)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if err := d.project(ctx, repository, root.RootIssueNumber, root.VersionID, pressure.Reason); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (d Dispatcher) hostPressure(ctx context.Context) (HostPressure, error) {
	if d.HostPressure == nil {
		return HostPressure{}, nil
	}
	reason, err := d.HostPressure.Inspect(ctx)
	if err != nil {
		return HostPressure{}, fmt.Errorf("inspect host pressure: %w", err)
	}
	return HostPressure{Reason: reason}, nil
}

func (d Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d Dispatcher) project(ctx context.Context, repository string, rootNumber int64, versionID, pauseReason string) error {
	projection, err := d.Store.PlanProjectionAt(ctx, versionID, d.now())
	if err != nil {
		return err
	}
	projection.DispatchPaused = pauseReason
	return d.Projector.ProjectPlan(ctx, repository, rootNumber, projection, "")
}

// Recover reprojects durable runtime state after a control-plane restart. It
// never claims a ticket and therefore cannot create a duplicate Worker Run.
func (d Dispatcher) Recover(ctx context.Context, repository string, rootNumber int64) error {
	if d.Store == nil || d.Reader == nil || d.Projector == nil {
		return fmt.Errorf("scheduler dependencies are incomplete")
	}
	snapshot, err := d.Reader.ReadPlan(ctx, repository, rootNumber)
	if err != nil {
		return err
	}
	version, err := d.Store.CurrentVersion(ctx, repository, snapshot.Root.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	if err := d.reconcileVersionLocal(ctx, version.ID, now); err != nil {
		return err
	}
	projection, err := d.Store.PlanProjectionAt(ctx, version.ID, now)
	if err != nil {
		return err
	}
	return d.Projector.ProjectPlan(ctx, repository, rootNumber, projection, "")
}

func (d Dispatcher) reconcileVersionLocal(ctx context.Context, versionID string, now time.Time) error {
	if d.Recovery == nil {
		return nil
	}
	expired, err := d.Store.ExpiredAgentRecoveryRuns(ctx, versionID, now)
	if err != nil {
		return err
	}
	for _, run := range expired {
		if err := d.Recovery.IsolateContainer(ctx, run.Claim.RunID); err != nil {
			return fmt.Errorf("isolate expired worker container %s: %w", run.Claim.RunID, err)
		}
		if err := d.Store.ReconcileMissingRecoveryRun(ctx, run, "Run Lease expired during restart recovery", now); err != nil && !errors.Is(err, store.ErrInvalidClaim) {
			return err
		}
	}
	runs, err := d.Store.ActiveRecoveryRuns(ctx, versionID, now)
	if err != nil {
		return err
	}
	for _, run := range runs {
		containerRunning, err := d.Recovery.ContainerRunning(ctx, run.Claim.RunID)
		if err != nil {
			return fmt.Errorf("recover worker container %s: %w", run.Claim.RunID, err)
		}
		workspaceAvailable, err := d.Recovery.WorkspaceAvailable(ctx, run.Session)
		if err != nil {
			return fmt.Errorf("recover ticket workspace %s: %w", run.Claim.SessionID, err)
		}
		if containerRunning && workspaceAvailable {
			continue
		}
		if d.ProvisionSession != nil && run.Kind == store.RunAgent {
			_, provisionErr := d.ProvisionSession(ctx, store.SessionProvisioning{SessionID: run.Claim.SessionID, Existing: true, WorkspacePath: run.Session.WorkspacePath, CodexStatePath: run.Session.CodexStatePath, CurrentRunID: run.Claim.RunID})
			if provisionErr != nil {
				var authenticationFailure *store.SessionAuthenticationFailure
				if !errors.As(provisionErr, &authenticationFailure) {
					return fmt.Errorf("recover Ticket Session authentication %s: %w", run.Claim.SessionID, provisionErr)
				}
				if err := d.Store.RecordRecoveryAuthenticationFailure(ctx, run, authenticationFailure.DiagnosticsPath, now); err != nil {
					return fmt.Errorf("record Ticket Session authentication failure %s: %w", run.Claim.SessionID, err)
				}
				continue
			}
		}
		reason := "worker container is not running"
		if !workspaceAvailable {
			reason = "ticket workspace is unavailable"
		}
		if err := d.Store.ReconcileMissingRecoveryRun(ctx, run, reason, now); err != nil {
			return err
		}
	}
	return nil
}
