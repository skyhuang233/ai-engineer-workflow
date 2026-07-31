package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

// PlanProjection derives the human-facing status table from durable runtime
// facts. Expired leases are intentionally shown as queued/reclaimable while
// their old Worker Run remains in the database for fencing and audit.
func (s *Store) PlanProjection(ctx context.Context, versionID string) (plan.Projection, error) {
	return s.PlanProjectionAt(ctx, versionID, time.Now().UTC())
}

func (s *Store) PlanProjectionAt(ctx context.Context, versionID string, now time.Time) (plan.Projection, error) {
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM plan_versions WHERE version_id = ?`, versionID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return plan.Projection{}, ErrNotFound
	} else if err != nil {
		return plan.Projection{}, err
	}
	projection := plan.Projection{VersionID: versionID, State: projectionState(state)}
	now = now.UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT t.issue_id, t.issue_number, t.title, COALESCE(rt.delivered, t.delivered), COALESCE(rt.state, ''),
COALESCE(s.owner, ''), COALESCE(s.session_id, ''), COALESCE(r.run_id, ''),
COALESCE(r.state, ''), COALESCE(l.generation, 0), COALESCE(l.state, ''), COALESCE(l.expires_at, '')
FROM plan_tickets t
LEFT JOIN ticket_runtime rt ON rt.version_id = t.version_id AND rt.issue_id = t.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
LEFT JOIN worker_runs r ON r.run_id = s.current_run_id
LEFT JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE t.version_id = ?`, versionID)
	if err != nil {
		return plan.Projection{}, err
	}
	defer rows.Close()
	byID := make(map[int64]int)
	for rows.Next() {
		var ticket plan.ProjectionTicket
		var issueID int64
		var delivered int
		var runtimeState string
		var runState, leaseState, expiresText string
		if err := rows.Scan(&issueID, &ticket.Number, &ticket.Title, &delivered, &runtimeState, &ticket.Owner, &ticket.SessionID, &ticket.RunID, &runState, &ticket.LeaseGeneration, &leaseState, &expiresText); err != nil {
			return plan.Projection{}, err
		}
		ticket.State = "Queued"
		if delivered != 0 {
			ticket.State = "Delivered"
			ticket.Owner = ""
			ticket.RunID = ""
			ticket.LeaseGeneration = 0
		} else if runtimeState == plan.StateRunning && runState == RunRunning && leaseState == LeaseActive {
			expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
			if parseErr == nil && expiresAt.After(now) {
				ticket.State = "Running"
			} else {
				ticket.Owner = ""
				ticket.RunID = ""
				ticket.LeaseGeneration = 0
			}
		} else {
			ticket.Owner = ""
			ticket.RunID = ""
			ticket.LeaseGeneration = 0
		}
		byID[issueID] = len(projection.Tickets)
		projection.Tickets = append(projection.Tickets, ticket)
	}
	if err := rows.Err(); err != nil {
		return plan.Projection{}, err
	}
	dependencyRows, err := s.db.QueryContext(ctx, `SELECT blocked_issue_id, blocker_issue_id FROM plan_dependencies WHERE version_id = ?`, versionID)
	if err != nil {
		return plan.Projection{}, err
	}
	defer dependencyRows.Close()
	for dependencyRows.Next() {
		var blocked, blocker int64
		if err := dependencyRows.Scan(&blocked, &blocker); err != nil {
			return plan.Projection{}, err
		}
		if index, ok := byID[blocked]; ok {
			projection.Tickets[index].Blockers = append(projection.Tickets[index].Blockers, blocker)
		}
	}
	return projection, dependencyRows.Err()
}

func projectionState(state string) string {
	if state == StateActive {
		return "Active"
	}
	return "Building"
}
