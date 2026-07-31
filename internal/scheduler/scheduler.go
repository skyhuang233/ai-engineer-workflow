package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type Dispatcher struct {
	Store           *store.Store
	Reader          plan.SnapshotReader
	Projector       plan.RootProjector
	MaxParallelRuns int
	LeaseTTL        time.Duration
	Now             func() time.Time
}

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
	now := time.Time{}
	if d.Now != nil {
		now = d.Now()
	}
	claim, err := d.Store.ClaimReady(ctx, store.ClaimRequest{
		VersionID:       version.ID,
		TicketID:        ticketID,
		Owner:           owner,
		MaxParallelRuns: d.MaxParallelRuns,
		LeaseTTL:        d.LeaseTTL,
		Now:             now,
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
	if err := d.Projector.UpdatePlanProjection(ctx, repository, rootNumber, projection); err != nil {
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
	projection, err := d.Store.PlanProjectionAt(ctx, version.ID, now)
	if err != nil {
		return err
	}
	return d.Projector.UpdatePlanProjection(ctx, repository, rootNumber, projection)
}
