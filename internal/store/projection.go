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
	var frozen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM plan_freezes WHERE version_id = ?`, versionID).Scan(&frozen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return plan.Projection{}, err
	}
	if frozen != 0 {
		projection.State = "Needs Attention"
	}
	now = now.UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT t.issue_id, t.issue_number, t.title, COALESCE(rt.delivered, t.delivered), COALESCE(rt.state, ''),
COALESCE(s.owner, ''), COALESCE(s.session_id, ''), COALESCE(r.run_id, ''),
COALESCE(r.state, ''), COALESCE(l.generation, 0), COALESCE(l.state, ''), COALESCE(l.expires_at, ''),
COALESCE(td.pull_request_number, 0), COALESCE(s.accepted_commit, ''),
CASE WHEN td.pull_request_number > 0 THEN 'not run' ELSE '' END,
MAX(COALESCE(rt.updated_at, ''), COALESCE(s.updated_at, ''), COALESCE(r.started_at, ''), COALESCE(r.finished_at, ''), COALESCE(td.updated_at, ''))
FROM plan_tickets t
LEFT JOIN ticket_runtime rt ON rt.version_id = t.version_id AND rt.issue_id = t.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
LEFT JOIN worker_runs r ON r.run_id = s.current_run_id
LEFT JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
LEFT JOIN ticket_deliveries td ON td.version_id = t.version_id AND td.issue_id = t.issue_id
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
		if err := rows.Scan(&issueID, &ticket.Number, &ticket.Title, &delivered, &runtimeState, &ticket.Owner, &ticket.SessionID, &ticket.RunID, &runState, &ticket.LeaseGeneration, &leaseState, &expiresText, &ticket.PullRequest, &ticket.Revision, &ticket.GateResult, &ticket.LastActivity); err != nil {
			return plan.Projection{}, err
		}
		ticket.State = "Queued"
		if delivered != 0 {
			ticket.State = "Delivered"
			ticket.Owner = ""
			ticket.RunID = ""
			ticket.LeaseGeneration = 0
		} else if runtimeState == plan.StateNeedsAttention {
			ticket.State = "Needs Attention"
			ticket.Owner = ""
			ticket.RunID = ""
			ticket.LeaseGeneration = 0
		} else if runtimeState == plan.StateWaitingReview {
			ticket.State = "Waiting Review"
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
	dependencyRows, err := s.db.QueryContext(ctx, `SELECT d.blocked_issue_id, blocker.issue_number
FROM plan_dependencies d
JOIN plan_tickets blocker ON blocker.version_id = d.version_id AND blocker.issue_id = d.blocker_issue_id
WHERE d.version_id = ?`, versionID)
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
	if err := dependencyRows.Err(); err != nil {
		return plan.Projection{}, err
	}
	questionRows, err := s.db.QueryContext(ctx, `SELECT question_id, prompt FROM workflow_questions WHERE version_id = ? AND state = 'open' ORDER BY question_id`, versionID)
	if err != nil {
		return plan.Projection{}, err
	}
	defer questionRows.Close()
	for questionRows.Next() {
		var question plan.WorkflowQuestion
		if err := questionRows.Scan(&question.ID, &question.Prompt); err != nil {
			return plan.Projection{}, err
		}
		projection.Questions = append(projection.Questions, question)
	}
	if err := questionRows.Err(); err != nil {
		return plan.Projection{}, err
	}
	return projection, nil
}

func projectionState(state string) string {
	if state == StateActive {
		return "Active"
	}
	return "Building"
}
