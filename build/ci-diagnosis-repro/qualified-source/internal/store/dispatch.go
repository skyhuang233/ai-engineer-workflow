package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

// DispatchCandidate is one durable, globally ordered ticket eligible for a
// new Worker Run. The plan root identity is included so callers can project
// the Plan Root that owns a successful claim.
type DispatchCandidate struct {
	VersionID       string
	RootIssueID     int64
	RootIssueNumber int64
	Ticket          plan.FrontierTicket
}

// PlanRoot identifies an active Delivery Plan whose status may need a
// control-plane projection.
type PlanRoot struct {
	VersionID       string
	RootIssueID     int64
	RootIssueNumber int64
}

// ActivePlanRoots returns the current active plans in stable human-visible
// order. It deliberately excludes historical and terminal plan versions.
func (s *Store) ActivePlanRoots(ctx context.Context, repository string) ([]PlanRoot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.version_id, p.root_issue_id, p.root_issue_number
FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND `+currentActivePlanPredicate+`
ORDER BY p.root_issue_number, p.root_issue_id`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []PlanRoot
	for rows.Next() {
		var root PlanRoot
		if err := rows.Scan(&root.VersionID, &root.RootIssueID, &root.RootIssueNumber); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

// GlobalReadyFrontier previews the next global dispatches. The persisted
// fairness cursor determines which active plan is considered first, then the
// frontier proceeds in round-robin turns across plans.
func (s *Store) GlobalReadyFrontier(ctx context.Context, repository string, maxParallelRuns int, now time.Time) ([]DispatchCandidate, error) {
	if repository == "" || maxParallelRuns <= 0 {
		return nil, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	frontier, err := globalReadyFrontierTx(ctx, tx, repository, maxParallelRuns, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return frontier, nil
}

// ClaimNextReady picks the next candidate through the persisted fairness
// cursor. ClaimReady then repeats eligibility, ownership, and global capacity
// checks in its write transaction before it records both the run and cursor.
func (s *Store) ClaimNextReady(ctx context.Context, repository, owner string, maxParallelRuns, maxAttempts int, leaseTTL time.Duration, now time.Time, provision SessionProvisioner) (TicketClaim, error) {
	if repository == "" || owner == "" || maxParallelRuns <= 0 {
		return TicketClaim{}, ErrInvalidClaim
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	frontier, err := s.GlobalReadyFrontier(ctx, repository, maxParallelRuns, now)
	if err != nil {
		return TicketClaim{}, err
	}
	if len(frontier) == 0 {
		return TicketClaim{}, ErrNoReadyTickets
	}
	candidate := frontier[0]
	return s.claimReady(ctx, ClaimRequest{
		VersionID:           candidate.VersionID,
		TicketID:            candidate.Ticket.IssueID,
		Owner:               owner,
		MaxParallelRuns:     maxParallelRuns,
		MaxAttempts:         maxAttempts,
		LeaseTTL:            leaseTTL,
		Now:                 now,
		ProvisionSession:    provision,
		fairnessRepository:  repository,
		fairnessRootIssueID: candidate.RootIssueID,
		fairnessRootNumber:  candidate.RootIssueNumber,
	})
}

type globalPlanFrontier struct {
	root    PlanRoot
	tickets []plan.FrontierTicket
}

func globalReadyFrontierTx(ctx context.Context, tx *sql.Tx, repository string, maxParallelRuns int, now time.Time) ([]DispatchCandidate, error) {
	activeRuns, err := activeRunCountTx(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	if activeRuns >= maxParallelRuns {
		return nil, ErrCapacity
	}
	rows, err := tx.QueryContext(ctx, `SELECT v.version_id, p.root_issue_id, p.root_issue_number
FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND `+currentActivePlanPredicate+`
ORDER BY p.root_issue_number, p.root_issue_id`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []globalPlanFrontier
	for rows.Next() {
		var candidate globalPlanFrontier
		if err := rows.Scan(&candidate.root.VersionID, &candidate.root.RootIssueID, &candidate.root.RootIssueNumber); err != nil {
			return nil, err
		}
		snapshot, err := loadFrontierTx(ctx, tx, candidate.root.VersionID, now)
		if err != nil {
			return nil, err
		}
		// This is a global capacity-sized preview. It intentionally does not
		// reserve a per-plan share; round-robin below provides fairness.
		snapshot.MaxParallel = activeRuns + (maxParallelRuns - activeRuns)
		candidate.tickets = plan.ReadyFrontier(snapshot)
		if len(candidate.tickets) > 0 {
			plans = append(plans, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortGlobalPlans(plans)
	if len(plans) == 0 {
		return nil, nil
	}
	lastRootID, err := dispatchFairnessCursorTx(ctx, tx, repository)
	if err != nil {
		return nil, err
	}
	start := 0
	for i, candidate := range plans {
		if candidate.root.RootIssueID == lastRootID {
			start = (i + 1) % len(plans)
			break
		}
	}
	capacity := maxParallelRuns - activeRuns
	result := make([]DispatchCandidate, 0, capacity)
	for len(result) < capacity {
		progressed := false
		for offset := 0; offset < len(plans) && len(result) < capacity; offset++ {
			index := (start + offset) % len(plans)
			candidate := &plans[index]
			if len(candidate.tickets) == 0 {
				continue
			}
			result = append(result, DispatchCandidate{VersionID: candidate.root.VersionID, RootIssueID: candidate.root.RootIssueID, RootIssueNumber: candidate.root.RootIssueNumber, Ticket: candidate.tickets[0]})
			candidate.tickets = candidate.tickets[1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return result, nil
}

func dispatchFairnessCursorTx(ctx context.Context, tx *sql.Tx, repository string) (int64, error) {
	var rootID int64
	err := tx.QueryRowContext(ctx, `SELECT last_root_issue_id FROM dispatch_fairness WHERE repository = ?`, repository).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rootID, err
}

func recordDispatchFairnessTx(ctx context.Context, tx *sql.Tx, repository string, rootID int64, now time.Time) error {
	if repository == "" || rootID == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO dispatch_fairness(repository, last_root_issue_id, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(repository) DO UPDATE SET last_root_issue_id = excluded.last_root_issue_id, updated_at = excluded.updated_at`, repository, rootID, formatTimestamp(now))
	return err
}

// Keep a compiler-visible sort use close to the ordering contract. It guards
// against future query changes accidentally relying on database row order.
func sortGlobalPlans(plans []globalPlanFrontier) {
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].root.RootIssueNumber == plans[j].root.RootIssueNumber {
			return plans[i].root.RootIssueID < plans[j].root.RootIssueID
		}
		return plans[i].root.RootIssueNumber < plans[j].root.RootIssueNumber
	})
}
