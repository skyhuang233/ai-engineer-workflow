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
	WorkspaceAvailable(context.Context, store.TicketSession) (bool, error)
}

type HostPressureInspector interface {
	Unsafe(context.Context) (bool, error)
}

// Claim reads the Plan Root only for the current human body and repository
// identity. Store.ClaimReady remains the atomic authority; projection failure
// leaves the durable claim intact for the next reconcile pass.
func (d Dispatcher) Claim(ctx context.Context, repository string, rootNumber, ticketID int64, owner string) (store.TicketClaim, error) {
	if d.Store == nil || d.Reader == nil || d.Projector == nil {
		return store.TicketClaim{}, fmt.Errorf("scheduler dependencies are incomplete")
	}
	if d.HostPressure != nil {
		unsafe, err := d.HostPressure.Unsafe(ctx)
		if err != nil {
			return store.TicketClaim{}, fmt.Errorf("inspect host pressure: %w", err)
		}
		if unsafe {
			return store.TicketClaim{}, store.ErrCapacity
		}
	}
	snapshot, err := d.Reader.ReadPlan(ctx, repository, rootNumber)
	if err != nil {
		return store.TicketClaim{}, err
	}
	version, err := d.Store.CurrentVersion(ctx, repository, snapshot.Root.ID)
	if err != nil {
		return store.TicketClaim{}, err
	}
	now := time.Time{}
	if d.Now != nil {
		now = d.Now()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
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
	projectionNow := time.Now().UTC()
	if d.Now != nil {
		projectionNow = d.Now().UTC()
	}
	projection, err := d.Store.PlanProjectionAt(ctx, version.ID, projectionNow)
	if err != nil {
		return claim, err
	}
	if err := d.Projector.ProjectPlan(ctx, repository, rootNumber, projection, ""); err != nil {
		return claim, fmt.Errorf("project ticket claim: %w", err)
	}
	return claim, nil
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
	if d.Recovery != nil {
		runs, err := d.Store.ActiveRecoveryRuns(ctx, version.ID, now)
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
				_, provisionErr := d.ProvisionSession(ctx, store.SessionProvisioning{
					SessionID: run.Claim.SessionID, Existing: true, WorkspacePath: run.Session.WorkspacePath,
					CodexStatePath: run.Session.CodexStatePath, CurrentRunID: run.Claim.RunID,
				})
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
	}
	projection, err := d.Store.PlanProjectionAt(ctx, version.ID, now)
	if err != nil {
		return err
	}
	return d.Projector.ProjectPlan(ctx, repository, rootNumber, projection, "")
}
