package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type AmendmentEdge struct {
	BlockedTicketID int64 `json:"blocked_ticket_id"`
	BlockerTicketID int64 `json:"blocker_ticket_id"`
}

// PlanAmendment is the only structured route for changing an active Delivery
// Plan. The target is computed and stored here; no GitHub graph is changed.
type PlanAmendment struct {
	VersionID          string          `json:"version_id"`
	TicketID           int64           `json:"ticket_id"`
	Summary            string          `json:"summary"`
	AddTickets         []plan.Issue    `json:"add_tickets,omitempty"`
	RemoveTicketIDs    []int64         `json:"remove_ticket_ids,omitempty"`
	AddDependencies    []AmendmentEdge `json:"add_dependencies,omitempty"`
	RemoveDependencies []AmendmentEdge `json:"remove_dependencies,omitempty"`
}

type PlanAmendmentProposal struct {
	ID         string
	QuestionID string
	Impact     string
}

type amendmentRecord struct {
	Target plan.Snapshot `json:"target"`
}

type amendmentDecision struct {
	Action string `json:"action"`
}

func (s *Store) ProposePlanAmendment(ctx context.Context, amendment PlanAmendment, now time.Time, isolated ...TicketClaim) (PlanAmendmentProposal, error) {
	if amendment.VersionID == "" || amendment.TicketID == 0 || strings.TrimSpace(amendment.Summary) == "" {
		return PlanAmendmentProposal{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlanAmendmentProposal{}, err
	}
	defer tx.Rollback()
	if err := amendmentSourceActiveTx(ctx, tx, amendment.VersionID); err != nil {
		return PlanAmendmentProposal{}, err
	}
	source, err := amendmentSnapshotTx(ctx, tx, amendment.VersionID)
	if err != nil {
		return PlanAmendmentProposal{}, err
	}
	target, changed, err := amendedSnapshot(source, amendment)
	if err != nil {
		return PlanAmendmentProposal{}, err
	}
	affected := amendmentAffected(source, target, changed)
	affectedIDs := make(map[int64]bool, len(affected))
	for _, issueID := range affected {
		affectedIDs[issueID] = true
	}
	if err := requireDeliveryIsolationTx(ctx, tx, amendment.VersionID, affectedIDs, isolated); err != nil {
		return PlanAmendmentProposal{}, err
	}
	impact, err := amendmentImpactTx(ctx, tx, amendment.VersionID, source, target, affected)
	if err != nil {
		return PlanAmendmentProposal{}, err
	}
	if err := ensureWorkflowQuestionTx(ctx, tx, source.Repository, amendment.VersionID, amendment.TicketID, "plan_amendment", impact, now); err != nil {
		return PlanAmendmentProposal{}, err
	}
	var questionID string
	if err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = 'plan_amendment' AND state = 'open'`, source.Repository, amendment.VersionID, amendment.TicketID).Scan(&questionID); err != nil {
		return PlanAmendmentProposal{}, err
	}
	encoded, err := json.Marshal(amendmentRecord{Target: target})
	if err != nil {
		return PlanAmendmentProposal{}, err
	}
	id, err := randomID("amendment-")
	if err != nil {
		return PlanAmendmentProposal{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_amendments(amendment_id, version_id, issue_id, question_id, proposal_json, impact_report, state, proposed_at) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`, id, amendment.VersionID, amendment.TicketID, questionID, string(encoded), impact, formatTimestamp(now)); err != nil {
		return PlanAmendmentProposal{}, err
	}
	for _, issueID := range affected {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_amendment_pauses(amendment_id, version_id, issue_id) VALUES (?, ?, ?)`, id, amendment.VersionID, issueID); err != nil {
			return PlanAmendmentProposal{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = 'running' AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)`, formatTimestamp(now), amendment.VersionID, issueID); err != nil {
			return PlanAmendmentProposal{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = 'active' AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)`, amendment.VersionID, issueID); err != nil {
			return PlanAmendmentProposal{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET owner = '', updated_at = ? WHERE version_id = ? AND issue_id = ?`, formatTimestamp(now), amendment.VersionID, issueID); err != nil {
			return PlanAmendmentProposal{}, err
		}
	}
	if _, err := s.queueWorkflowInboxProjectionTx(ctx, tx, source.Repository, now); err != nil {
		return PlanAmendmentProposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlanAmendmentProposal{}, err
	}
	return PlanAmendmentProposal{ID: id, QuestionID: questionID, Impact: impact}, nil
}

func amendmentSourceActiveTx(ctx context.Context, tx *sql.Tx, versionID string) error {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id WHERE v.version_id = ? AND `+currentActiveUnfrozenPlanPredicate+`)`, versionID).Scan(&active); err != nil {
		return err
	}
	if active == 0 {
		return ErrNotReady
	}
	return nil
}

func amendmentSnapshotTx(ctx context.Context, tx *sql.Tx, versionID string) (plan.Snapshot, error) {
	var snapshot plan.Snapshot
	err := tx.QueryRowContext(ctx, `SELECT p.repository, p.root_issue_id, p.root_issue_number FROM plans p JOIN plan_versions v ON v.plan_id = p.id WHERE v.version_id = ?`, versionID).Scan(&snapshot.Repository, &snapshot.Root.ID, &snapshot.Root.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrNotFound
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.Root.Labels = []string{plan.PlanLabel}
	rows, err := tx.QueryContext(ctx, `SELECT t.issue_id, t.issue_number, t.title, t.body, t.state, COALESCE(r.delivered, t.delivered) FROM plan_tickets t LEFT JOIN ticket_runtime r ON r.version_id = t.version_id AND r.issue_id = t.issue_id WHERE t.version_id = ? ORDER BY t.issue_id`, versionID)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	byID := map[int64]plan.Issue{}
	for rows.Next() {
		var ticket plan.Issue
		var delivered int
		if err := rows.Scan(&ticket.ID, &ticket.Number, &ticket.Title, &ticket.Body, &ticket.State, &delivered); err != nil {
			return snapshot, err
		}
		ticket.Labels, ticket.Delivered = []string{plan.TicketLabel}, delivered != 0
		snapshot.Children, byID[ticket.ID] = append(snapshot.Children, ticket), ticket
	}
	if err := rows.Err(); err != nil {
		return snapshot, err
	}
	snapshot.BlockedBy = map[int64][]plan.Issue{}
	edges, err := tx.QueryContext(ctx, `SELECT blocked_issue_id, blocker_issue_id FROM plan_dependencies WHERE version_id = ?`, versionID)
	if err != nil {
		return snapshot, err
	}
	defer edges.Close()
	for edges.Next() {
		var blocked, blocker int64
		if err := edges.Scan(&blocked, &blocker); err != nil {
			return snapshot, err
		}
		snapshot.BlockedBy[blocked] = append(snapshot.BlockedBy[blocked], byID[blocker])
	}
	return snapshot, edges.Err()
}

func amendedSnapshot(source plan.Snapshot, amendment PlanAmendment) (plan.Snapshot, map[int64]bool, error) {
	byID := map[int64]plan.Issue{}
	for _, ticket := range source.Tickets() {
		byID[ticket.ID] = ticket
	}
	if _, exists := byID[amendment.TicketID]; !exists {
		return plan.Snapshot{}, nil, ErrNotFound
	}
	changed := map[int64]bool{amendment.TicketID: true}
	for _, id := range amendment.RemoveTicketIDs {
		ticket, exists := byID[id]
		if !exists || ticket.Delivered {
			return plan.Snapshot{}, nil, ErrInvalidClaim
		}
		delete(byID, id)
		changed[id] = true
	}
	for _, ticket := range amendment.AddTickets {
		if ticket.ID == 0 || ticket.Number == 0 || strings.TrimSpace(ticket.Title) == "" {
			return plan.Snapshot{}, nil, ErrInvalidClaim
		}
		if _, exists := byID[ticket.ID]; exists {
			return plan.Snapshot{}, nil, ErrInvalidClaim
		}
		ticket.Labels = []string{plan.TicketLabel}
		byID[ticket.ID] = ticket
		changed[ticket.ID] = true
	}
	edges := snapshotEdges(source)
	for _, edge := range amendment.RemoveDependencies {
		if !edges[edge] {
			return plan.Snapshot{}, nil, ErrInvalidClaim
		}
		delete(edges, edge)
		changed[edge.BlockedTicketID], changed[edge.BlockerTicketID] = true, true
	}
	for _, edge := range amendment.AddDependencies {
		if edge.BlockedTicketID == 0 || edge.BlockerTicketID == 0 || edge.BlockedTicketID == edge.BlockerTicketID || byID[edge.BlockedTicketID].ID == 0 || byID[edge.BlockerTicketID].ID == 0 || edges[edge] {
			return plan.Snapshot{}, nil, ErrInvalidClaim
		}
		edges[edge] = true
		changed[edge.BlockedTicketID], changed[edge.BlockerTicketID] = true, true
	}
	if len(changed) == 1 {
		return plan.Snapshot{}, nil, ErrInvalidClaim
	}
	target := plan.Snapshot{Repository: source.Repository, Root: source.Root, BlockedBy: map[int64][]plan.Issue{}}
	for _, ticket := range byID {
		target.Children = append(target.Children, ticket)
	}
	sort.Slice(target.Children, func(i, j int) bool { return target.Children[i].ID < target.Children[j].ID })
	for edge := range edges {
		target.BlockedBy[edge.BlockedTicketID] = append(target.BlockedBy[edge.BlockedTicketID], byID[edge.BlockerTicketID])
	}
	if err := target.Validate(); err != nil {
		return plan.Snapshot{}, nil, err
	}
	return target, changed, nil
}

func amendmentAffected(source, target plan.Snapshot, changed map[int64]bool) []int64 {
	children := map[int64][]int64{}
	for _, snapshot := range []plan.Snapshot{source, target} {
		for blocked, blockers := range snapshot.BlockedBy {
			for _, blocker := range blockers {
				children[blocker.ID] = append(children[blocker.ID], blocked)
			}
		}
	}
	seen, queue := map[int64]bool{}, make([]int64, 0, len(changed))
	for id := range changed {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, children[id]...)
	}
	var affected []int64
	for _, ticket := range source.Tickets() {
		if seen[ticket.ID] && !ticket.Delivered {
			affected = append(affected, ticket.ID)
		}
	}
	sort.Slice(affected, func(i, j int) bool { return affected[i] < affected[j] })
	return affected
}

func amendmentImpactTx(ctx context.Context, tx *sql.Tx, versionID string, source, target plan.Snapshot, affected []int64) (string, error) {
	sourceTickets, targetTickets := issueMap(source.Tickets()), issueMap(target.Tickets())
	var report strings.Builder
	report.WriteString("Plan Amendment pending human approval.\n\n")
	fmt.Fprintf(&report, "Nodes added: %s\n", changedIssues(targetTickets, sourceTickets))
	fmt.Fprintf(&report, "Nodes removed: %s\n", changedIssues(sourceTickets, targetTickets))
	fmt.Fprintf(&report, "Edges added: %s\n", changedEdges(snapshotEdges(target), snapshotEdges(source), targetTickets))
	fmt.Fprintf(&report, "Edges removed: %s\n", changedEdges(snapshotEdges(source), snapshotEdges(target), sourceTickets))
	fmt.Fprintf(&report, "Affected tickets: %s\n", issueNumbers(affected, sourceTickets))
	runs, prs, candidates, err := amendmentRuntimeCountsTx(ctx, tx, versionID, affected)
	if err != nil {
		return "", err
	}
	if runs == 0 && prs == 0 {
		report.WriteString("Worker Runs/PRs: none\n")
	} else {
		fmt.Fprintf(&report, "Worker Runs/PRs: %d running Worker Run(s), %d pull request(s)\n", runs, prs)
	}
	if candidates == 0 {
		report.WriteString("Candidate revisions: none\n")
	} else {
		fmt.Fprintf(&report, "Candidate revisions: %d may be invalidated\n", candidates)
	}
	dependents, err := amendmentDependentsTx(ctx, tx, versionID, affected)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&report, "Cross-plan dependents: %s\n", dependents)
	return report.String(), nil
}

func issueMap(tickets []plan.Issue) map[int64]plan.Issue {
	result := map[int64]plan.Issue{}
	for _, ticket := range tickets {
		result[ticket.ID] = ticket
	}
	return result
}
func snapshotEdges(snapshot plan.Snapshot) map[AmendmentEdge]bool {
	result := map[AmendmentEdge]bool{}
	for blocked, blockers := range snapshot.BlockedBy {
		for _, blocker := range blockers {
			result[AmendmentEdge{BlockedTicketID: blocked, BlockerTicketID: blocker.ID}] = true
		}
	}
	return result
}
func changedIssues(left, right map[int64]plan.Issue) string {
	var ids []int64
	for id := range left {
		if _, exists := right[id]; !exists {
			ids = append(ids, id)
		}
	}
	return issueNumbers(ids, left)
}
func issueNumbers(ids []int64, tickets map[int64]plan.Issue) string {
	if len(ids) == 0 {
		return "none"
	}
	sort.Slice(ids, func(i, j int) bool { return tickets[ids[i]].Number < tickets[ids[j]].Number })
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, fmt.Sprintf("#%d", tickets[id].Number))
	}
	return strings.Join(values, ", ")
}
func changedEdges(left, right map[AmendmentEdge]bool, tickets map[int64]plan.Issue) string {
	var values []string
	for edge := range left {
		if !right[edge] {
			values = append(values, fmt.Sprintf("#%d blocked by #%d", tickets[edge.BlockedTicketID].Number, tickets[edge.BlockerTicketID].Number))
		}
	}
	sort.Strings(values)
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func amendmentRuntimeCountsTx(ctx context.Context, tx *sql.Tx, versionID string, affected []int64) (int, int, int, error) {
	runs, prs, candidates := 0, 0, 0
	for _, issueID := range affected {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE s.version_id = ? AND s.issue_id = ? AND r.state = 'running'`, versionID, issueID).Scan(&count); err != nil {
			return 0, 0, 0, err
		}
		runs += count
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticket_deliveries WHERE version_id = ? AND issue_id = ? AND pull_request_number > 0`, versionID, issueID).Scan(&count); err != nil {
			return 0, 0, 0, err
		}
		prs += count
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM candidate_revisions c JOIN ticket_sessions s ON s.session_id = c.session_id WHERE s.version_id = ? AND s.issue_id = ?`, versionID, issueID).Scan(&count); err != nil {
			return 0, 0, 0, err
		}
		candidates += count
	}
	return runs, prs, candidates, nil
}

func amendmentDependentsTx(ctx context.Context, tx *sql.Tx, versionID string, affected []int64) (string, error) {
	var values []string
	for _, issueID := range affected {
		rows, err := tx.QueryContext(ctx, `SELECT p.repository, p.root_issue_number, t.issue_number FROM plan_dependencies d JOIN plan_tickets t ON t.version_id = d.version_id AND t.issue_id = d.blocked_issue_id JOIN plan_versions v ON v.version_id = d.version_id JOIN plans p ON p.id = v.plan_id WHERE d.blocker_issue_id = ? AND d.version_id <> ? AND p.current_version_id = v.version_id ORDER BY p.repository, p.root_issue_number, t.issue_number`, issueID, versionID)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var repository string
			var root, ticket int64
			if err := rows.Scan(&repository, &root, &ticket); err != nil {
				rows.Close()
				return "", err
			}
			values = append(values, fmt.Sprintf("%s plan #%d ticket #%d", repository, root, ticket))
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
	}
	if len(values) == 0 {
		return "none", nil
	}
	return strings.Join(values, ", "), nil
}

func (s *Store) resolvePlanAmendmentTx(ctx context.Context, tx *sql.Tx, questionID, sourceVersionID, action string, now time.Time, isolated []TicketClaim) error {
	var amendmentID, raw, state string
	err := tx.QueryRowContext(ctx, `SELECT amendment_id, proposal_json, state FROM plan_amendments WHERE question_id = ? AND version_id = ?`, questionID, sourceVersionID).Scan(&amendmentID, &raw, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "pending" {
		return ErrNotReady
	}
	switch action {
	case "reject":
		if _, err := tx.ExecContext(ctx, `DELETE FROM plan_amendment_pauses WHERE amendment_id = ?`, amendmentID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE plan_amendments SET state = 'rejected', decided_at = ? WHERE amendment_id = ?`, formatTimestamp(now), amendmentID)
		return err
	case "approve":
		var record amendmentRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return err
		}
		return s.applyPlanAmendmentTx(ctx, tx, amendmentID, sourceVersionID, record.Target, now, isolated)
	default:
		return ErrInvalidClaim
	}
}

func (s *Store) applyPlanAmendmentTx(ctx context.Context, tx *sql.Tx, amendmentID, sourceVersionID string, target plan.Snapshot, now time.Time, isolated []TicketClaim) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if err := requireDeliveryIsolationTx(ctx, tx, sourceVersionID, nil, isolated); err != nil {
		return err
	}
	var planID int64
	var repository string
	if err := tx.QueryRowContext(ctx, `SELECT p.id, p.repository FROM plans p JOIN plan_versions v ON v.plan_id = p.id WHERE v.version_id = ? AND p.current_version_id = ?`, sourceVersionID, sourceVersionID).Scan(&planID, &repository); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		return err
	}
	fingerprint, err := target.Fingerprint()
	if err != nil {
		return err
	}
	versionID := "pv-" + fingerprint
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_versions(version_id, plan_id, fingerprint, source_revision, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`, versionID, planID, fingerprint, "amendment:"+amendmentID, StateActive, formatTimestamp(now)); err != nil {
		return err
	}
	for _, ticket := range target.Tickets() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_tickets(version_id, issue_id, issue_number, title, body, state, delivered) VALUES (?, ?, ?, ?, ?, ?, ?)`, versionID, ticket.ID, ticket.Number, ticket.Title, ticket.Body, ticket.State, boolInt(ticket.Delivered)); err != nil {
			return err
		}
		state := plan.StateQueued
		if ticket.Delivered {
			state = plan.StateDelivered
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_runtime(version_id, issue_id, state, delivered, updated_at) VALUES (?, ?, ?, ?, ?)`, versionID, ticket.ID, state, boolInt(ticket.Delivered), formatTimestamp(now)); err != nil {
			return err
		}
	}
	for blocked, blockers := range target.BlockedBy {
		for _, blocker := range blockers {
			if _, err := tx.ExecContext(ctx, `INSERT INTO plan_dependencies(version_id, blocked_issue_id, blocker_issue_id) VALUES (?, ?, ?)`, versionID, blocked, blocker.ID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = 'running' AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, formatTimestamp(now), sourceVersionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = 'active' AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, sourceVersionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET owner = '', updated_at = ? WHERE version_id = ?`, formatTimestamp(now), sourceVersionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET current_version_id = ?, state = ?, updated_at = ? WHERE id = ?`, versionID, StateActive, formatTimestamp(now), planID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plan_amendment_pauses WHERE amendment_id = ?`, amendmentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plan_amendments SET state = 'approved', applied_version_id = ?, decided_at = ? WHERE amendment_id = ?`, versionID, formatTimestamp(now), amendmentID); err != nil {
		return err
	}
	_, err = s.queueWorkflowInboxProjectionTransitionTx(ctx, tx, repository, now)
	return err
}
