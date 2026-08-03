package plan

import (
	"context"
	"fmt"
)

// SnapshotReader is intentionally small so the activation path can be tested
// without a live GitHub repository.
type SnapshotReader interface {
	ReadPlan(context.Context, string, int64) (Snapshot, error)
}

type RootProjector interface {
	ProjectPlan(context.Context, string, int64, Projection, string) error
}

type VersionStore interface {
	BeginActivation(context.Context, Snapshot, string, string) (Version, error)
	MarkActive(context.Context, string) error
}

// Version is the narrow store contract used by the plan domain. The concrete
// store package aliases this shape through an adapter to avoid a dependency
// from the core domain onto SQLite.
type Version struct {
	ID              string
	Repository      string
	RootIssueID     int64
	RootIssueNumber int64
	Fingerprint     string
	SourceRevision  string
	State           string
}

type Activator struct {
	Reader    SnapshotReader
	Projector RootProjector
	Store     VersionStore
}

func (a Activator) Activate(ctx context.Context, repository string, rootNumber int64) (Version, error) {
	snapshot, err := a.Reader.ReadPlan(ctx, repository, rootNumber)
	if err != nil {
		return Version{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return Version{}, err
	}
	// Validate the marker shape before creating the durable projecting version.
	// The real block is rendered again after the store allocates its version ID.
	if _, err := RenderProjection(snapshot.Root.Body, Projection{VersionID: "pending", State: "Building"}); err != nil {
		return Version{}, err
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		return Version{}, err
	}
	version, err := a.Store.BeginActivation(ctx, snapshot, fingerprint, snapshot.Root.UpdatedAt)
	if err != nil {
		return Version{}, err
	}
	projectionState := "Building"
	if version.State != "projecting" {
		projectionState = displayState(version.State)
	}
	projection, err := projectionFor(snapshot, version, projectionState)
	if err != nil {
		return Version{}, err
	}
	if version.State == "projecting" {
		if err := a.Projector.ProjectPlan(ctx, repository, rootNumber, projection, ActiveLabel); err != nil {
			return Version{}, fmt.Errorf("project plan root: %w", err)
		}
		if err := a.Store.MarkActive(ctx, version.ID); err != nil {
			return Version{}, err
		}
		version.State = "active"
	}
	projection.State = displayState(version.State)
	if err := a.Projector.ProjectPlan(ctx, repository, rootNumber, projection, ""); err != nil {
		return Version{}, fmt.Errorf("reconcile active plan root: %w", err)
	}
	return version, nil
}

func displayState(state string) string {
	switch state {
	case "completed":
		return "Completed"
	case "cancelled":
		return "Cancelled"
	case "active":
		return "Active"
	default:
		return "Building"
	}
}

func projectionFor(snapshot Snapshot, version Version, state string) (Projection, error) {
	tickets := make(map[int64]ProjectionTicket)
	for _, ticket := range snapshot.Tickets() {
		tickets[ticket.ID] = ProjectionTicket{Number: ticket.Number, Title: ticket.Title}
	}
	for blocked, blockers := range snapshot.BlockedBy {
		projected, ok := tickets[blocked]
		if !ok {
			return Projection{}, fmt.Errorf("projection has unknown ticket %d", blocked)
		}
		for _, blocker := range blockers {
			projected.Blockers = append(projected.Blockers, blocker.Number)
		}
		tickets[blocked] = projected
	}
	ordered := make([]ProjectionTicket, 0, len(tickets))
	for _, ticket := range tickets {
		ordered = append(ordered, ticket)
	}
	return Projection{VersionID: version.ID, State: state, Tickets: ordered}, nil
}
