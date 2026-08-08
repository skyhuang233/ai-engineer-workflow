package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type WorkflowQuestion struct {
	ID           string
	Repository   string
	VersionID    string
	IssueID      int64
	Kind         string
	Prompt       string
	State        string
	Answer       string
	RootNumber   int64
	PlanNumbers  []int64
	TicketNumber int64
	PullRequest  int64
	Commit       string
	Diagnostics  string
	Evidence     string
}

type closedPlanDecision struct {
	Action      string                `json:"action"`
	Replacement *replacementReference `json:"replacement,omitempty"`
}

type replacementReference struct {
	VersionID       string `json:"version_id,omitempty"`
	TicketID        int64  `json:"ticket_id,omitempty"`
	PlanRootIssueID int64  `json:"plan_root_issue_id,omitempty"`
}

func (s *Store) OpenWorkflowQuestions(ctx context.Context, repository string, rootNumber int64) ([]WorkflowQuestion, error) {
	return openWorkflowQuestions(ctx, s.db, repository, rootNumber, false)
}

func (s *Store) ActiveWorkflowQuestions(ctx context.Context, repository string) ([]WorkflowQuestion, error) {
	return openWorkflowQuestions(ctx, s.db, repository, 0, true)
}

func (s *Store) WorkflowInboxQuestions(ctx context.Context, repository string) ([]WorkflowQuestion, error) {
	return workflowInboxQuestions(ctx, s.db, repository)
}

func (s *Store) WorkflowInboxProjection(ctx context.Context, repository string) ([]WorkflowQuestion, string, []string, error) {
	questions, version, versionIDs, err := s.WorkflowInboxProjectionState(ctx, repository)
	if err == nil && len(versionIDs) == 0 {
		err = ErrNoActiveDeliveryPlan
	}
	return questions, version, versionIDs, err
}

func (s *Store) WorkflowInboxProjectionState(ctx context.Context, repository string) ([]WorkflowQuestion, string, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", nil, err
	}
	defer tx.Rollback()
	versionIDs, err := workflowInboxDeliveryPlanVersions(ctx, tx, repository)
	if err != nil {
		return nil, "", nil, err
	}
	questions, err := workflowInboxQuestions(ctx, tx, repository)
	if err != nil {
		return nil, "", nil, err
	}
	version, err := workflowInboxProjectionVersion(questions)
	if err != nil {
		return nil, "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", nil, err
	}
	return questions, version, versionIDs, nil
}

const workflowQuestionSelect = `SELECT q.question_id, q.repository, q.version_id, q.issue_id, q.kind, q.prompt, q.state, q.answer,
CASE WHEN q.kind = 'inbox_delivery_recovery' THEN 0 ELSE p.root_issue_number END,
COALESCE(qc.ticket_number, t.issue_number, 0), COALESCE(qc.pull_request_number, d.pull_request_number, 0), COALESCE(qc.accepted_commit, s.accepted_commit, ''), COALESCE(qc.diagnostics_path, rd.diagnostics_path, ''), COALESCE(qc.candidate_evidence, c.structured_output, ''),
COALESCE((
    SELECT json_group_array(origin.root_issue_number)
    FROM (
        SELECT DISTINCT origin_plan.root_issue_number
        FROM inbox_delivery_recovery_questions recovery
        JOIN json_each(recovery.plan_version_ids_json) provenance
        JOIN plan_versions origin_version ON origin_version.version_id = provenance.value
        JOIN plans origin_plan ON origin_plan.id = origin_version.plan_id
        WHERE recovery.question_id = q.question_id
        ORDER BY origin_plan.root_issue_number
    ) origin
), '[]')
FROM workflow_questions q
JOIN plan_versions v ON v.version_id = q.version_id
JOIN plans p ON p.id = v.plan_id
LEFT JOIN workflow_question_contexts qc ON qc.question_id = q.question_id
LEFT JOIN plan_tickets t ON t.version_id = q.version_id AND t.issue_id = q.issue_id
LEFT JOIN ticket_deliveries d ON d.version_id = q.version_id AND d.issue_id = q.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = q.version_id AND s.issue_id = q.issue_id
LEFT JOIN run_diagnostics rd ON rd.run_id = s.current_run_id
LEFT JOIN candidate_revisions c ON c.run_id = s.current_run_id
`

func workflowInboxQuestions(ctx context.Context, querier workflowQuestionQuerier, repository string) ([]WorkflowQuestion, error) {
	rows, err := querier.QueryContext(ctx, workflowQuestionSelect+`WHERE q.repository = ? AND q.state = 'open' AND ((`+workflowInboxPlanPredicate+`) OR (`+workflowInboxRecoveryQuestionPredicate+`))
ORDER BY q.created_at, q.question_id`, repository)
	if err != nil {
		return nil, err
	}
	return scanWorkflowQuestions(rows)
}

type workflowQuestionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func openWorkflowQuestions(ctx context.Context, querier workflowQuestionQuerier, repository string, rootNumber int64, activeOnly bool) ([]WorkflowQuestion, error) {
	active := 0
	if activeOnly {
		active = 1
	}
	rows, err := querier.QueryContext(ctx, workflowQuestionSelect+`WHERE q.repository = ? AND (? = 0 OR p.root_issue_number = ?) AND q.state = 'open'
AND (? = 0 OR (`+currentActivePlanPredicate+`))
ORDER BY q.created_at, q.question_id`, repository, rootNumber, rootNumber, active)
	if err != nil {
		return nil, err
	}
	return scanWorkflowQuestions(rows)
}

func scanWorkflowQuestions(rows *sql.Rows) ([]WorkflowQuestion, error) {
	defer rows.Close()
	var questions []WorkflowQuestion
	for rows.Next() {
		var question WorkflowQuestion
		var planNumbersJSON string
		if err := rows.Scan(&question.ID, &question.Repository, &question.VersionID, &question.IssueID, &question.Kind, &question.Prompt, &question.State, &question.Answer, &question.RootNumber, &question.TicketNumber, &question.PullRequest, &question.Commit, &question.Diagnostics, &question.Evidence, &planNumbersJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(planNumbersJSON), &question.PlanNumbers); err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return questions, rows.Err()
}

type GitHubPollCursor struct {
	Repository            string
	LastSuccessAt         time.Time
	LastFullReconcileAt   time.Time
	ConsecutiveFailures   int
	FailureKind           GitHubPollFailureKind
	RecoveryState         GitHubPollRecoveryState
	RecoveryPlanVersionID string
	NextAttemptAt         time.Time
}

func (c GitHubPollCursor) NeedsAttention() bool {
	return c.FailureKind == GitHubPollFailureUnrecoverable && c.RecoveryState == GitHubPollRecoveryConsumed
}

func (c GitHubPollCursor) HasBootstrapRecoveryCandidate(minimumFailures int) bool {
	return minimumFailures > 0 &&
		c.ConsecutiveFailures >= minimumFailures &&
		c.FailureKind == GitHubPollFailurePreActivationInboxConflict &&
		c.RecoveryPlanVersionID != "" &&
		(c.RecoveryState == GitHubPollRecoveryAvailable || c.RecoveryState == GitHubPollRecoveryClaimed)
}

type GitHubPollFailureKind string
type GitHubPollRecoveryState string
type GitHubPollBootstrapRecoveryDisposition string
type GitHubPollTerminalFailureDisposition string

const (
	GitHubPollFailurePreActivationInboxConflict GitHubPollFailureKind                  = "pre_activation_inbox_conflict"
	GitHubPollFailureRetryable                  GitHubPollFailureKind                  = "retryable"
	GitHubPollFailureUnrecoverable              GitHubPollFailureKind                  = "unrecoverable"
	GitHubPollRecoveryAvailable                 GitHubPollRecoveryState                = "available"
	GitHubPollRecoveryClaimed                   GitHubPollRecoveryState                = "claimed"
	GitHubPollRecoveryCompleted                 GitHubPollRecoveryState                = "completed"
	GitHubPollRecoveryConsumed                  GitHubPollRecoveryState                = "consumed"
	GitHubPollBootstrapRecoveryUnavailable      GitHubPollBootstrapRecoveryDisposition = "unavailable"
	GitHubPollBootstrapRecoveryActive           GitHubPollBootstrapRecoveryDisposition = "active"
	GitHubPollBootstrapRecoveryProjecting       GitHubPollBootstrapRecoveryDisposition = "projecting"
	GitHubPollBootstrapRecoveryStale            GitHubPollBootstrapRecoveryDisposition = "stale"
	GitHubPollTerminalFailureResolved           GitHubPollTerminalFailureDisposition   = "resolved"
	GitHubPollTerminalFailureRetryable          GitHubPollTerminalFailureDisposition   = "retryable"
	GitHubPollTerminalFailureNeedsAttention     GitHubPollTerminalFailureDisposition   = "needs_attention"
)

func (s *Store) GitHubPollCursor(ctx context.Context, repository string) (GitHubPollCursor, error) {
	return scanGitHubPollCursor(s.db.QueryRowContext(ctx, `SELECT repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, next_attempt_at
FROM github_poll_cursors WHERE repository = ?`, repository))
}

type githubPollCursorScanner interface {
	Scan(...any) error
}

func scanGitHubPollCursor(row githubPollCursorScanner) (GitHubPollCursor, error) {
	var cursor GitHubPollCursor
	var success, full, next string
	err := row.Scan(&cursor.Repository, &success, &full, &cursor.ConsecutiveFailures, &cursor.FailureKind, &cursor.RecoveryState, &cursor.RecoveryPlanVersionID, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubPollCursor{}, ErrNotFound
	}
	if err != nil {
		return GitHubPollCursor{}, err
	}
	var parseErr error
	if success != "" {
		cursor.LastSuccessAt, parseErr = time.Parse(time.RFC3339Nano, success)
		if parseErr != nil {
			return GitHubPollCursor{}, parseErr
		}
	}
	if full != "" {
		cursor.LastFullReconcileAt, parseErr = time.Parse(time.RFC3339Nano, full)
		if parseErr != nil {
			return GitHubPollCursor{}, parseErr
		}
	}
	if next != "" {
		cursor.NextAttemptAt, parseErr = time.Parse(time.RFC3339Nano, next)
		if parseErr != nil {
			return GitHubPollCursor{}, parseErr
		}
	}
	return cursor, nil
}

func (s *Store) HasActiveDeliveryPlan(ctx context.Context, repository string) (bool, error) {
	_, active, err := s.ActiveDeliveryPlanVersion(ctx, repository)
	return active, err
}

func (s *Store) HasWorkflowInboxDeliveryPlan(ctx context.Context, repository string) (bool, error) {
	versionIDs, err := workflowInboxDeliveryPlanVersions(ctx, s.db, repository)
	return len(versionIDs) > 0, err
}

func (s *Store) ActiveDeliveryPlanVersion(ctx context.Context, repository string) (string, bool, error) {
	versionIDs, err := s.ActiveDeliveryPlanVersions(ctx, repository)
	if err != nil {
		return "", false, err
	}
	if len(versionIDs) == 0 {
		return "", false, nil
	}
	return versionIDs[0], true, nil
}

func (s *Store) ActiveDeliveryPlanVersions(ctx context.Context, repository string) ([]string, error) {
	return activeDeliveryPlanVersions(ctx, s.db, repository)
}

func activeDeliveryPlanVersions(ctx context.Context, querier workflowQuestionQuerier, repository string) ([]string, error) {
	rows, err := querier.QueryContext(ctx, `SELECT v.version_id FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND `+currentActivePlanPredicate+` ORDER BY v.version_id`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versionIDs []string
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			return nil, err
		}
		versionIDs = append(versionIDs, versionID)
	}
	return versionIDs, rows.Err()
}

func workflowInboxDeliveryPlanVersions(ctx context.Context, querier workflowQuestionQuerier, repository string) ([]string, error) {
	rows, err := querier.QueryContext(ctx, `SELECT version_id FROM (
    SELECT v.version_id FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
    WHERE p.repository = ? AND (`+workflowInboxPlanPredicate+`)
    UNION
    SELECT provenance.value
    FROM inbox_delivery_recovery_questions recovery
    JOIN workflow_questions recovery_question ON recovery_question.question_id = recovery.question_id
    JOIN json_each(recovery.plan_version_ids_json) provenance
    WHERE recovery_question.repository = ? AND recovery_question.state = 'open'
) ORDER BY version_id`, repository, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versionIDs []string
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			return nil, err
		}
		versionIDs = append(versionIDs, versionID)
	}
	return versionIDs, rows.Err()
}

func (s *Store) HasProjectingDeliveryPlan(ctx context.Context, repository string) (bool, error) {
	_, projecting, err := s.ProjectingDeliveryPlanVersion(ctx, repository)
	return projecting, err
}

func (s *Store) ProjectingDeliveryPlanVersion(ctx context.Context, repository string) (string, bool, error) {
	var count int
	var versionID string
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(v.version_id), '') FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND p.current_version_id = v.version_id AND p.state = ? AND v.state = ?
AND NOT EXISTS (SELECT 1 FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id)
AND NOT EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id)`, repository, StateProjecting, StateProjecting).Scan(&count, &versionID)
	if err != nil {
		return "", false, err
	}
	return versionID, count == 1, nil
}

func (s *Store) IsProjectingDeliveryPlanVersion(ctx context.Context, repository, versionID string) (bool, error) {
	var projecting int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
    WHERE p.repository = ? AND v.version_id = ? AND p.state = ? AND v.state = ?
    AND NOT EXISTS (SELECT 1 FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id)
    AND NOT EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id)
)`, repository, versionID, StateProjecting, StateProjecting).Scan(&projecting)
	return projecting != 0, err
}

func requireGitHubPollLeaseTx(ctx context.Context, tx *sql.Tx, repository, token string, leaseNow time.Time) error {
	if token == "" {
		return ErrFencingConflict
	}
	if leaseNow.IsZero() {
		return ErrFencingConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors SET updated_at = updated_at
WHERE repository = ? AND lease_token = ? AND lease_expires_at > ?`, repository, token, formatTimestamp(leaseNow.UTC()))
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) acquireGitHubPollMutationLease(ctx context.Context, repository string) (string, time.Time, error) {
	if repository == "" {
		return "", time.Time{}, ErrInvalidClaim
	}
	token, err := randomID("github-poll-mutation-")
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	timestamp := formatTimestamp(now)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, next_attempt_at, updated_at)
VALUES (?, ?, ?) ON CONFLICT(repository) DO NOTHING`, repository, timestamp, timestamp); err != nil {
		return "", time.Time{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE github_poll_cursors
SET lease_token = ?, lease_expires_at = ?, updated_at = ?
WHERE repository = ? AND (lease_token = '' OR lease_expires_at <= ?)`, token, formatTimestamp(now.Add(30*time.Second)), timestamp, repository, timestamp)
	if err != nil {
		return "", time.Time{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return "", time.Time{}, err
	}
	if updated != 1 {
		return "", time.Time{}, ErrFencingConflict
	}
	return token, now, nil
}

func (s *Store) releaseGitHubPollMutationLease(ctx context.Context, repository, token string) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return s.ReleaseGitHubPollLease(releaseCtx, repository, token, time.Now().UTC())
}

// AcquireGitHubPollLease atomically combines readiness admission with exclusive
// ownership. The lease is persisted so concurrent control-plane processes cannot
// both pass NextAttemptAt and begin GitHub access for the same repository.
func (s *Store) AcquireGitHubPollLease(ctx context.Context, repository, token string, now time.Time, ttl time.Duration) error {
	if repository == "" || token == "" || ttl <= 0 {
		return ErrInvalidClaim
	}
	now = now.UTC()
	timestamp := formatTimestamp(now)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, next_attempt_at, updated_at)
VALUES (?, ?, ?) ON CONFLICT(repository) DO NOTHING`, repository, timestamp, timestamp); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE github_poll_cursors
SET lease_token = ?, lease_expires_at = ?, updated_at = ?
WHERE repository = ? AND next_attempt_at <= ? AND (lease_token = '' OR lease_expires_at <= ?)`,
		token, formatTimestamp(now.Add(ttl)), timestamp, repository, timestamp, timestamp)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrNotReady
	}
	return nil
}

func (s *Store) RenewGitHubPollLease(ctx context.Context, repository, token string, now time.Time, ttl time.Duration) error {
	if repository == "" || token == "" || ttl <= 0 {
		return ErrFencingConflict
	}
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE github_poll_cursors
SET lease_expires_at = ?, updated_at = ?
WHERE repository = ? AND lease_token = ? AND lease_expires_at > ?`,
		formatTimestamp(now.Add(ttl)), formatTimestamp(now), repository, token, formatTimestamp(now))
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) ReleaseGitHubPollLease(ctx context.Context, repository, token string, now time.Time) error {
	if repository == "" || token == "" {
		return ErrFencingConflict
	}
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE github_poll_cursors
SET lease_token = '', lease_expires_at = '', updated_at = ?
WHERE repository = ? AND lease_token = ?`, formatTimestamp(now), repository, token)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) ClaimGitHubPollBootstrapRecovery(ctx context.Context, repository string, minimumFailures int, now time.Time) (bool, error) {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return false, err
	}
	claimed, claimErr := s.ClaimGitHubPollBootstrapRecoveryLeased(ctx, repository, minimumFailures, now, token, leaseNow)
	return claimed, errors.Join(claimErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) ClaimGitHubPollBootstrapRecoveryLeased(ctx context.Context, repository string, minimumFailures int, now time.Time, leaseToken string, leaseNow time.Time) (bool, error) {
	if minimumFailures < 1 {
		return false, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET recovery_state = ?, updated_at = ?
WHERE repository = ? AND consecutive_failures >= ? AND failure_kind = ? AND recovery_state = ?
AND recovery_plan_version_id != ''
AND EXISTS (
    SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
    WHERE p.repository = github_poll_cursors.repository
    AND v.version_id = github_poll_cursors.recovery_plan_version_id
    AND p.state = ? AND v.state = ?
    AND NOT EXISTS (SELECT 1 FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id)
    AND NOT EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id)
)`, GitHubPollRecoveryClaimed, formatTimestamp(now), repository, minimumFailures, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, StateProjecting, StateProjecting)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return updated == 1, nil
}

func (s *Store) ResolveGitHubPollBootstrapRecoveryLeased(ctx context.Context, repository string, minimumFailures int, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollBootstrapRecoveryDisposition, error) {
	if minimumFailures < 1 {
		return GitHubPollBootstrapRecoveryUnavailable, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GitHubPollBootstrapRecoveryUnavailable, err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return GitHubPollBootstrapRecoveryUnavailable, err
	}
	var cursor GitHubPollCursor
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id
FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&cursor.ConsecutiveFailures, &cursor.FailureKind, &cursor.RecoveryState, &cursor.RecoveryPlanVersionID)
	if err != nil {
		return GitHubPollBootstrapRecoveryUnavailable, err
	}
	if !cursor.HasBootstrapRecoveryCandidate(minimumFailures) {
		return GitHubPollBootstrapRecoveryUnavailable, nil
	}
	versionID := cursor.RecoveryPlanVersionID
	var planState, versionState string
	var terminal, completed int
	err = tx.QueryRowContext(ctx, `SELECT p.state, v.state,
EXISTS (SELECT 1 FROM plan_terminal_states state WHERE state.version_id = v.version_id),
EXISTS (SELECT 1 FROM completed_plan_versions state WHERE state.version_id = v.version_id)
FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND v.version_id = ?`, repository, versionID).Scan(&planState, &versionState, &terminal, &completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return GitHubPollBootstrapRecoveryUnavailable, err
	}
	disposition := GitHubPollBootstrapRecoveryStale
	if err == nil && terminal == 0 && completed == 0 {
		switch {
		case planState == StateActive && versionState == StateActive:
			disposition = GitHubPollBootstrapRecoveryActive
		case planState == StateProjecting && versionState == StateProjecting:
			disposition = GitHubPollBootstrapRecoveryProjecting
		}
	}
	switch disposition {
	case GitHubPollBootstrapRecoveryActive:
		_, err = tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0, failure_kind = '', recovery_state = ?, attempted_plan_version_ids_json = '[]', next_attempt_at = ?, updated_at = ?
WHERE repository = ? AND failure_kind = ? AND recovery_state IN (?, ?) AND recovery_plan_version_id = ?`,
			GitHubPollRecoveryCompleted, formatTimestamp(now), formatTimestamp(now), repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, GitHubPollRecoveryClaimed, versionID)
	case GitHubPollBootstrapRecoveryProjecting:
		_, err = tx.ExecContext(ctx, `UPDATE github_poll_cursors SET recovery_state = ?, updated_at = ?
WHERE repository = ? AND failure_kind = ? AND recovery_state IN (?, ?) AND recovery_plan_version_id = ?`,
			GitHubPollRecoveryClaimed, formatTimestamp(now), repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, GitHubPollRecoveryClaimed, versionID)
	case GitHubPollBootstrapRecoveryStale:
		_, err = tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0, failure_kind = ?, recovery_state = ?, recovery_plan_version_id = '', attempted_plan_version_ids_json = '[]', next_attempt_at = ?, updated_at = ?
WHERE repository = ? AND failure_kind = ? AND recovery_state IN (?, ?) AND recovery_plan_version_id = ?`,
			GitHubPollFailureRetryable, GitHubPollRecoveryConsumed, formatTimestamp(now), formatTimestamp(now), repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, GitHubPollRecoveryClaimed, versionID)
	}
	if err != nil {
		return GitHubPollBootstrapRecoveryUnavailable, err
	}
	if err := tx.Commit(); err != nil {
		return GitHubPollBootstrapRecoveryUnavailable, err
	}
	return disposition, nil
}

func (s *Store) RecoverGitHubPollAfterBootstrap(ctx context.Context, repository string, now time.Time) (bool, error) {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return false, err
	}
	recovered, recoverErr := s.RecoverGitHubPollAfterBootstrapLeased(ctx, repository, now, token, leaseNow)
	return recovered, errors.Join(recoverErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) RecoverGitHubPollAfterBootstrapLeased(ctx context.Context, repository string, now time.Time, leaseToken string, leaseNow time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0, failure_kind = '', recovery_state = ?, attempted_plan_version_ids_json = '[]', next_attempt_at = ?, updated_at = ?
WHERE repository = ? AND failure_kind = ? AND recovery_state = ?
AND recovery_plan_version_id != ''
AND EXISTS (
    SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
    WHERE p.repository = github_poll_cursors.repository
    AND v.version_id = github_poll_cursors.recovery_plan_version_id
    AND `+currentActivePlanPredicate+`
)`, GitHubPollRecoveryCompleted, formatTimestamp(now), formatTimestamp(now), repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryClaimed)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return updated == 1, nil
}

func (s *Store) ConsumeGitHubPollBootstrapEligibility(ctx context.Context, repository string, now time.Time) error {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	consumeErr := s.consumeGitHubPollBootstrapEligibility(ctx, repository, now, token, leaseNow)
	return errors.Join(consumeErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) ConsumeGitHubPollBootstrapEligibilityLeased(ctx context.Context, repository, leaseToken string, now, leaseNow time.Time) error {
	return s.consumeGitHubPollBootstrapEligibility(ctx, repository, now, leaseToken, leaseNow)
}

func (s *Store) consumeGitHubPollBootstrapEligibility(ctx context.Context, repository string, now time.Time, leaseToken string, leaseNow time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET failure_kind = ?, recovery_state = ?, recovery_plan_version_id = '', updated_at = ?
WHERE repository = ? AND failure_kind = ? AND recovery_state IN (?, ?)`, GitHubPollFailureRetryable, GitHubPollRecoveryConsumed, formatTimestamp(now), repository, GitHubPollFailurePreActivationInboxConflict, GitHubPollRecoveryAvailable, GitHubPollRecoveryClaimed)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func resetGitHubPollForCredentialTx(ctx context.Context, tx *sql.Tx, repository string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0,
failure_kind = CASE WHEN failure_kind = ? AND recovery_state = ? THEN failure_kind ELSE ? END,
recovery_state = CASE WHEN failure_kind = ? AND recovery_state = ? THEN recovery_state ELSE ? END,
recovery_plan_version_id = CASE WHEN failure_kind = ? AND recovery_state = ? THEN recovery_plan_version_id ELSE '' END,
attempted_plan_version_ids_json = '[]',
next_attempt_at = ?, updated_at = ?
WHERE repository = ?`, GitHubPollFailureUnrecoverable, GitHubPollRecoveryConsumed, GitHubPollFailureRetryable,
		GitHubPollFailureUnrecoverable, GitHubPollRecoveryConsumed, GitHubPollRecoveryConsumed,
		GitHubPollFailureUnrecoverable, GitHubPollRecoveryConsumed,
		formatTimestamp(now), formatTimestamp(now), repository)
	return err
}

func (s *Store) MarkGitHubPollFailureUnrecoverable(ctx context.Context, repository string, now time.Time) error {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	markErr := s.MarkGitHubPollFailureUnrecoverableLeased(ctx, repository, now, token, leaseNow)
	return errors.Join(markErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) MarkGitHubPollFailureUnrecoverableLeased(ctx context.Context, repository string, now time.Time, leaseToken string, leaseNow time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return err
	}
	if err := markGitHubPollFailureUnrecoverableTx(ctx, tx, repository, "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func markGitHubPollFailureUnrecoverableTx(ctx context.Context, tx *sql.Tx, repository, recoveryPlanVersionID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, next_attempt_at, updated_at)
VALUES (?, 0, ?, ?, ?, ?, ?)
ON CONFLICT(repository) DO UPDATE SET failure_kind = excluded.failure_kind, recovery_state = excluded.recovery_state,
recovery_plan_version_id = CASE
    WHEN github_poll_cursors.failure_kind = excluded.failure_kind
      AND github_poll_cursors.recovery_state = excluded.recovery_state
      AND github_poll_cursors.recovery_plan_version_id != ''
    THEN github_poll_cursors.recovery_plan_version_id
    ELSE excluded.recovery_plan_version_id
END,
updated_at = excluded.updated_at`, repository, GitHubPollFailureUnrecoverable, GitHubPollRecoveryConsumed, recoveryPlanVersionID, formatTimestamp(now), formatTimestamp(now))
	return err
}

func (s *Store) RecordGitHubPollSuccess(ctx context.Context, repository string, now time.Time, fullReconcile bool) error {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	recordErr := s.RecordGitHubPollSuccessLeased(ctx, repository, now, fullReconcile, token, leaseNow)
	return errors.Join(recordErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) RecordGitHubPollSuccessLeased(ctx context.Context, repository string, now time.Time, fullReconcile bool, leaseToken string, leaseNow time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return err
	}
	if err := recordGitHubPollSuccessTx(ctx, tx, repository, now, fullReconcile); err != nil {
		return err
	}
	return tx.Commit()
}

func recordGitHubPollSuccessTx(ctx context.Context, tx *sql.Tx, repository string, now time.Time, fullReconcile bool) error {
	full := ""
	if fullReconcile {
		full = formatTimestamp(now)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, next_attempt_at, updated_at)
VALUES (?, ?, ?, 0, '', '', '', ?, ?)
ON CONFLICT(repository) DO UPDATE SET last_success_at = excluded.last_success_at,
last_full_reconcile_at = CASE WHEN excluded.last_full_reconcile_at = '' THEN github_poll_cursors.last_full_reconcile_at ELSE excluded.last_full_reconcile_at END,
consecutive_failures = 0, failure_kind = '', recovery_plan_version_id = '', attempted_plan_version_ids_json = '[]',
recovery_state = CASE WHEN github_poll_cursors.recovery_state = 'completed' THEN github_poll_cursors.recovery_state ELSE '' END,
next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at`, repository, formatTimestamp(now), full, formatTimestamp(now), formatTimestamp(now)); err != nil {
		return err
	}
	return nil
}

func (s *Store) DeferGitHubPoll(ctx context.Context, repository string, retryAt, now time.Time) error {
	_, err := s.DeferGitHubPollWithCursor(ctx, repository, retryAt, now)
	return err
}

func (s *Store) DeferGitHubPollWithCursor(ctx context.Context, repository string, retryAt, now time.Time) (GitHubPollCursor, error) {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return GitHubPollCursor{}, err
	}
	cursor, deferErr := s.DeferGitHubPollWithCursorLeased(ctx, repository, retryAt, now, token, leaseNow)
	return cursor, errors.Join(deferErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) DeferGitHubPollWithCursorLeased(ctx context.Context, repository string, retryAt, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	return s.DeferGitHubPollWithCursorForPlanAttemptsLeased(ctx, repository, nil, retryAt, now, leaseToken, leaseNow)
}

func (s *Store) DeferGitHubPollWithCursorForPlanAttemptsLeased(ctx context.Context, repository string, attemptedPlanVersionIDs []string, retryAt, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	if retryAt.IsZero() {
		return GitHubPollCursor{}, ErrInvalidClaim
	}
	return s.advanceGitHubPollFailureLeased(ctx, repository, now, GitHubPollFailureRetryable, "", attemptedPlanVersionIDs, retryAt, leaseToken, leaseNow)
}

func (s *Store) DeferGitHubPollBootstrapRecoveryLeased(ctx context.Context, repository, recoveryPlanVersionID string, retryAt, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	return s.DeferGitHubPollBootstrapRecoveryForPlanAttemptsLeased(ctx, repository, recoveryPlanVersionID, nil, retryAt, now, leaseToken, leaseNow)
}

func (s *Store) DeferGitHubPollBootstrapRecoveryForPlanAttemptsLeased(ctx context.Context, repository, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, retryAt, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	if retryAt.IsZero() {
		return GitHubPollCursor{}, ErrInvalidClaim
	}
	if recoveryPlanVersionID == "" {
		return GitHubPollCursor{}, ErrInvalidClaim
	}
	return s.advanceGitHubPollFailureLeased(ctx, repository, now, GitHubPollFailurePreActivationInboxConflict, recoveryPlanVersionID, attemptedPlanVersionIDs, retryAt, leaseToken, leaseNow)
}

func (s *Store) MarkRepositoryNeedsAttention(ctx context.Context, repository string, now time.Time) error {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	markErr := s.MarkRepositoryNeedsAttentionLeased(ctx, repository, now, token, leaseNow)
	return errors.Join(markErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) MarkRepositoryNeedsAttentionLeased(ctx context.Context, repository string, now time.Time, leaseToken string, leaseNow time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return err
	}
	if _, err := markRepositoryNeedsAttentionTx(ctx, tx, repository, "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttention(ctx context.Context, repository string, now time.Time) error {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	markErr := s.MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttentionLeased(ctx, repository, now, token, leaseNow)
	return errors.Join(markErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttentionLeased(ctx context.Context, repository string, now time.Time, leaseToken string, leaseNow time.Time) error {
	_, err := s.resolveGitHubPollTerminalFailureForPlanLeased(ctx, repository, "", nil, now, leaseToken, leaseNow, false)
	return err
}

func (s *Store) MarkGitHubPollFailureUnrecoverableAndRepositoryNeedsAttentionForPlanLeased(ctx context.Context, repository, recoveryPlanVersionID string, now time.Time, leaseToken string, leaseNow time.Time) error {
	_, err := s.resolveGitHubPollTerminalFailureForPlanLeased(ctx, repository, recoveryPlanVersionID, nil, now, leaseToken, leaseNow, false)
	return err
}

func (s *Store) ResolveGitHubPollTerminalFailureForPlanLeased(ctx context.Context, repository, recoveryPlanVersionID string, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollTerminalFailureDisposition, error) {
	return s.resolveGitHubPollTerminalFailureForPlanLeased(ctx, repository, recoveryPlanVersionID, nil, now, leaseToken, leaseNow, true)
}

func (s *Store) ResolveGitHubPollTerminalFailureForPlanAttemptsLeased(ctx context.Context, repository, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollTerminalFailureDisposition, error) {
	return s.resolveGitHubPollTerminalFailureForPlanLeased(ctx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, leaseToken, leaseNow, true)
}

func (s *Store) ResolveGitHubPollTerminalFailureForPlanAttemptsStrictLeased(ctx context.Context, repository, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, now time.Time, leaseToken string, leaseNow time.Time) (GitHubPollTerminalFailureDisposition, error) {
	return s.resolveGitHubPollTerminalFailureForPlanLeased(ctx, repository, recoveryPlanVersionID, attemptedPlanVersionIDs, now, leaseToken, leaseNow, false)
}

func (s *Store) resolveGitHubPollTerminalFailureForPlanLeased(ctx context.Context, repository, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, now time.Time, leaseToken string, leaseNow time.Time, retryUnowned bool) (GitHubPollTerminalFailureDisposition, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return "", err
	}
	if recoveryPlanVersionID == "" || len(attemptedPlanVersionIDs) == 0 {
		var persistedRecoveryPlanVersionID, attemptedJSON string
		err = tx.QueryRowContext(ctx, `SELECT recovery_plan_version_id, attempted_plan_version_ids_json
FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&persistedRecoveryPlanVersionID, &attemptedJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if err == nil {
			if recoveryPlanVersionID == "" {
				recoveryPlanVersionID = persistedRecoveryPlanVersionID
			}
			if len(attemptedPlanVersionIDs) == 0 {
				if err := json.Unmarshal([]byte(attemptedJSON), &attemptedPlanVersionIDs); err != nil {
					return "", err
				}
			}
		}
	}
	completed, err := allAttemptedPlanVersionsCompletedTx(ctx, tx, attemptedPlanVersionIDs)
	if err != nil {
		return "", err
	}
	if completed {
		if err := recordGitHubPollSuccessTx(ctx, tx, repository, now, false); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return GitHubPollTerminalFailureResolved, nil
	}
	owned, err := markRepositoryNeedsAttentionTx(ctx, tx, repository, recoveryPlanVersionID, now)
	if err != nil {
		return "", err
	}
	frozenOwned, err := markFrozenAttemptedPlansPollRecoveryTx(ctx, tx, repository, attemptedPlanVersionIDs, now)
	if err != nil {
		return "", err
	}
	owned = owned || frozenOwned
	if !owned {
		if !retryUnowned {
			if err := markGitHubPollFailureUnrecoverableTx(ctx, tx, repository, recoveryPlanVersionID, now); err != nil {
				return "", err
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return GitHubPollTerminalFailureNeedsAttention, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0, failure_kind = ?, recovery_state = ?, recovery_plan_version_id = '', attempted_plan_version_ids_json = '[]', updated_at = ?
WHERE repository = ?`, GitHubPollFailureRetryable, GitHubPollRecoveryConsumed, formatTimestamp(now), repository); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return GitHubPollTerminalFailureRetryable, nil
	}
	if err := markGitHubPollFailureUnrecoverableTx(ctx, tx, repository, recoveryPlanVersionID, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return GitHubPollTerminalFailureNeedsAttention, nil
}

func allAttemptedPlanVersionsCompletedTx(ctx context.Context, tx *sql.Tx, attemptedPlanVersionIDs []string) (bool, error) {
	if len(attemptedPlanVersionIDs) == 0 {
		return false, nil
	}
	seen := make(map[string]struct{}, len(attemptedPlanVersionIDs))
	for _, versionID := range attemptedPlanVersionIDs {
		if versionID == "" {
			return false, nil
		}
		if _, duplicate := seen[versionID]; duplicate {
			continue
		}
		seen[versionID] = struct{}{}
		var completed int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM completed_plan_versions WHERE version_id = ?)`, versionID).Scan(&completed); err != nil {
			return false, err
		}
		if completed == 0 {
			return false, nil
		}
	}
	return len(seen) > 0, nil
}

func markFrozenAttemptedPlansPollRecoveryTx(ctx context.Context, tx *sql.Tx, repository string, attemptedPlanVersionIDs []string, now time.Time) (bool, error) {
	owned := false
	seen := make(map[string]struct{}, len(attemptedPlanVersionIDs))
	for _, versionID := range attemptedPlanVersionIDs {
		if versionID == "" {
			continue
		}
		if _, duplicate := seen[versionID]; duplicate {
			continue
		}
		seen[versionID] = struct{}{}
		var frozen int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
    WHERE p.repository = ? AND v.version_id = ? AND (`+currentActivePlanPredicate+`)
      AND EXISTS (SELECT 1 FROM plan_freezes freeze WHERE freeze.version_id = v.version_id)
)`, repository, versionID).Scan(&frozen); err != nil {
			return false, err
		}
		if frozen == 0 {
			continue
		}
		if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 0, "poll_failure", "GitHub polling exhausted its retry budget. Reply with an id-addressed retry decision after resolving the GitHub access failure.", now); err != nil {
			return false, err
		}
		owned = true
	}
	return owned, nil
}

func markRepositoryNeedsAttentionTx(ctx context.Context, tx *sql.Tx, repository, recoveryPlanVersionID string, now time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT v.version_id FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND ((`+currentActiveUnfrozenPlanPredicate+`) OR (
    v.version_id = ? AND ((
        p.state = ? AND v.state = ?
        AND NOT EXISTS (SELECT 1 FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id)
        AND NOT EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id)
    ) OR EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id))
)) ORDER BY v.version_id`, repository, recoveryPlanVersionID, StateProjecting, StateProjecting)
	if err != nil {
		return false, err
	}
	var versionIDs []string
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			rows.Close()
			return false, err
		}
		versionIDs = append(versionIDs, versionID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, versionID := range versionIDs {
		if err := ensureWorkflowQuestionTx(ctx, tx, repository, versionID, 0, "poll_failure", "GitHub polling exhausted its retry budget. Reply with an id-addressed retry decision after resolving the GitHub access failure.", now); err != nil {
			return false, err
		}
		var pollQuestionID string
		if err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = 0 AND kind = 'poll_failure' AND state = 'open'`, repository, versionID).Scan(&pollQuestionID); err != nil {
			return false, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT issue_id FROM ticket_runtime WHERE version_id = ? AND delivered = 0 AND state != ?`, versionID, plan.StateNeedsAttention)
		if err != nil {
			return false, err
		}
		var issueIDs []int64
		for rows.Next() {
			var issueID int64
			if err := rows.Scan(&issueID); err != nil {
				rows.Close()
				return false, err
			}
			issueIDs = append(issueIDs, issueID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, err
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		for _, issueID := range issueIDs {
			if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, "GitHub polling exhausted its retry budget", now); err != nil {
				return false, err
			}
			var ticketQuestionID string
			if err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = 'needs_attention' AND state = 'open'`, repository, versionID, issueID).Scan(&ticketQuestionID); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO poll_failure_targets(poll_question_id, version_id, issue_id, ticket_question_id) VALUES (?, ?, ?, ?)
ON CONFLICT(poll_question_id, version_id, issue_id) DO NOTHING`, pollQuestionID, versionID, issueID, ticketQuestionID); err != nil {
				return false, err
			}
		}
	}
	return len(versionIDs) > 0, nil
}

func (s *Store) WorkflowQuestion(ctx context.Context, repository, questionID string) (WorkflowQuestion, error) {
	var question WorkflowQuestion
	err := s.db.QueryRowContext(ctx, `SELECT q.question_id, q.repository, q.version_id, q.issue_id, q.kind, q.prompt, q.state, q.answer, p.root_issue_number,
COALESCE(qc.ticket_number, t.issue_number, 0), COALESCE(qc.pull_request_number, d.pull_request_number, 0), COALESCE(qc.accepted_commit, s.accepted_commit, ''), COALESCE(qc.diagnostics_path, rd.diagnostics_path, ''), COALESCE(qc.candidate_evidence, c.structured_output, '')
FROM workflow_questions q
JOIN plan_versions v ON v.version_id = q.version_id
JOIN plans p ON p.id = v.plan_id
LEFT JOIN workflow_question_contexts qc ON qc.question_id = q.question_id
LEFT JOIN plan_tickets t ON t.version_id = q.version_id AND t.issue_id = q.issue_id
LEFT JOIN ticket_deliveries d ON d.version_id = q.version_id AND d.issue_id = q.issue_id
LEFT JOIN ticket_sessions s ON s.version_id = q.version_id AND s.issue_id = q.issue_id
LEFT JOIN run_diagnostics rd ON rd.run_id = s.current_run_id
LEFT JOIN candidate_revisions c ON c.run_id = s.current_run_id
WHERE q.question_id = ? AND q.repository = ?`, questionID, repository).
		Scan(&question.ID, &question.Repository, &question.VersionID, &question.IssueID, &question.Kind, &question.Prompt, &question.State, &question.Answer, &question.RootNumber, &question.TicketNumber, &question.PullRequest, &question.Commit, &question.Diagnostics, &question.Evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowQuestion{}, ErrNotFound
	}
	return question, err
}

func (s *Store) AnswerWorkflowQuestion(ctx context.Context, repository, questionID, answer string, now time.Time) error {
	if repository == "" || questionID == "" || answer == "" {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	answerErr := s.AnswerWorkflowQuestionLeased(ctx, repository, questionID, answer, now, token, leaseNow)
	return errors.Join(answerErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) AnswerWorkflowQuestionLeased(ctx context.Context, repository, questionID, answer string, now time.Time, leaseToken string, leaseNow time.Time) error {
	_, err := s.AnswerWorkflowQuestionAndQueueInboxProjectionLeased(ctx, repository, questionID, answer, now, leaseToken, leaseNow)
	return err
}

func (s *Store) AnswerWorkflowQuestionAndQueueInboxProjectionLeased(ctx context.Context, repository, questionID, answer string, now time.Time, leaseToken string, leaseNow time.Time) (DeliveryOutbox, error) {
	if repository == "" || questionID == "" || answer == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM workflow_questions WHERE question_id = ? AND repository = ?`, questionID, repository).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryOutbox{}, ErrNotFound
	}
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if kind == "inbox_delivery_recovery" {
		return s.RecoverUncertainInboxDeliveryQuestionLeased(ctx, repository, questionID, answer, now, leaseToken, leaseNow)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	defer tx.Rollback()
	if err := s.answerWorkflowQuestionTx(ctx, tx, repository, questionID, answer, now, leaseToken, leaseNow); err != nil {
		return DeliveryOutbox{}, err
	}
	outbox, err := s.queueWorkflowInboxProjectionTx(ctx, tx, repository, now)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return outbox, nil
}

func (s *Store) AnswerWorkflowQuestionAndQueueInboxProjection(ctx context.Context, repository, questionID, answer string, now time.Time) (DeliveryOutbox, error) {
	if repository == "" || questionID == "" || answer == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	outbox, answerErr := s.AnswerWorkflowQuestionAndQueueInboxProjectionLeased(ctx, repository, questionID, answer, now, token, leaseNow)
	return outbox, errors.Join(answerErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) QueueWorkflowInboxProjection(ctx context.Context, repository string, now time.Time) (DeliveryOutbox, error) {
	if repository == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	defer tx.Rollback()
	outbox, err := s.queueWorkflowInboxProjectionTx(ctx, tx, repository, now)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return outbox, nil
}

func (s *Store) QueueWorkflowInboxProjectionIfActive(ctx context.Context, repository string, now time.Time) (DeliveryOutbox, error) {
	if repository == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	defer tx.Rollback()
	outbox, err := s.queueWorkflowInboxProjectionIfActiveTx(ctx, tx, repository, now)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return outbox, nil
}

func (s *Store) queueWorkflowInboxProjectionTx(ctx context.Context, tx *sql.Tx, repository string, now time.Time) (DeliveryOutbox, error) {
	versionIDs, err := workflowInboxDeliveryPlanVersions(ctx, tx, repository)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	questions, err := workflowInboxQuestions(ctx, tx, repository)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	version, err := workflowInboxProjectionVersion(questions)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	generation, err := workflowInboxProjectionGenerationTx(ctx, tx, repository, version, versionIDs, now, true)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	return s.enqueueDeliveryTx(ctx, tx, DeliveryRequest{Operation: DeliveryProjectInbox, Repository: repository, InboxProjectionVersion: version, InboxProjectionGeneration: generation}, now)
}

func (s *Store) queueWorkflowInboxProjectionTransitionTx(ctx context.Context, tx *sql.Tx, repository string, now time.Time) (DeliveryOutbox, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_inbox_projections WHERE repository = ?)`, repository).Scan(&exists); err != nil {
		return DeliveryOutbox{}, err
	}
	if exists == 0 {
		return DeliveryOutbox{}, nil
	}
	return s.queueWorkflowInboxProjectionTx(ctx, tx, repository, now)
}

func (s *Store) queueWorkflowInboxProjectionIfActiveTx(ctx context.Context, tx *sql.Tx, repository string, now time.Time) (DeliveryOutbox, error) {
	versionIDs, err := workflowInboxDeliveryPlanVersions(ctx, tx, repository)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if len(versionIDs) == 0 {
		return DeliveryOutbox{}, nil
	}
	return s.queueWorkflowInboxProjectionTx(ctx, tx, repository, now)
}

func workflowInboxProjectionVersion(questions []WorkflowQuestion) (string, error) {
	encoded, err := json.Marshal(WorkflowQuestionProjections(questions))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func workflowInboxProjectionGenerationTx(ctx context.Context, tx *sql.Tx, repository, projectionVersion string, planVersionIDs []string, now time.Time, advance bool) (int64, error) {
	encodedPlanVersionIDs, err := json.Marshal(planVersionIDs)
	if err != nil {
		return 0, err
	}
	var generation int64
	var currentProjectionVersion, currentPlanVersionIDs string
	err = tx.QueryRowContext(ctx, `SELECT generation, projection_version, plan_version_ids_json FROM workflow_inbox_projections WHERE repository = ?`, repository).Scan(&generation, &currentProjectionVersion, &currentPlanVersionIDs)
	if errors.Is(err, sql.ErrNoRows) {
		if !advance {
			return 0, nil
		}
		generation = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_inbox_projections(repository, generation, projection_version, plan_version_ids_json, updated_at) VALUES (?, ?, ?, ?, ?)`, repository, generation, projectionVersion, string(encodedPlanVersionIDs), formatTimestamp(now))
		return generation, err
	}
	if err != nil {
		return 0, err
	}
	if currentProjectionVersion == projectionVersion && currentPlanVersionIDs == string(encodedPlanVersionIDs) {
		return generation, nil
	}
	if !advance {
		return generation, nil
	}
	generation++
	_, err = tx.ExecContext(ctx, `UPDATE workflow_inbox_projections SET generation = ?, projection_version = ?, plan_version_ids_json = ?, updated_at = ? WHERE repository = ?`, generation, projectionVersion, string(encodedPlanVersionIDs), formatTimestamp(now), repository)
	return generation, err
}

func WorkflowQuestionProjections(questions []WorkflowQuestion) []plan.WorkflowQuestion {
	projected := make([]plan.WorkflowQuestion, 0, len(questions))
	for _, question := range questions {
		projected = append(projected, plan.WorkflowQuestion{ID: question.ID, Prompt: question.Prompt, Repository: question.Repository, PlanNumber: question.RootNumber, PlanNumbers: question.PlanNumbers, TicketNumber: question.TicketNumber, PullRequest: question.PullRequest, Commit: question.Commit, Finding: question.Kind, Diagnostics: question.Diagnostics, Evidence: question.Evidence})
	}
	return projected
}

func (s *Store) answerWorkflowQuestionTx(ctx context.Context, tx *sql.Tx, repository, questionID, answer string, now time.Time, leaseToken string, leaseNow time.Time) error {
	var kind, versionID, state, priorAnswer string
	var issueID int64
	err := tx.QueryRowContext(ctx, `SELECT kind, version_id, issue_id, state, COALESCE(answer, '') FROM workflow_questions WHERE question_id = ? AND repository = ?`, questionID, repository).Scan(&kind, &versionID, &issueID, &state, &priorAnswer)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "open" {
		if (kind == "closed_unmerged_impact" || kind == "inbox_delivery_recovery" || kind == "plan_amendment") && state == "answered" && priorAnswer == answer {
			return nil
		}
		return ErrNotFound
	}
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return err
	}
	var eligible int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
    WHERE p.repository = ? AND v.version_id = ? AND ((`+workflowInboxPlanPredicate+`) OR EXISTS (
        SELECT 1 FROM inbox_delivery_recovery_questions recovery WHERE recovery.question_id = ?
    ))
)`, repository, versionID, questionID).Scan(&eligible); err != nil {
		return err
	}
	if eligible == 0 {
		return ErrNotFound
	}
	if kind == "quality_gate" {
		var resumable int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1
    FROM quality_gate_questions gate
    JOIN ticket_sessions s ON s.session_id = gate.session_id
    JOIN ticket_runtime runtime ON runtime.version_id = gate.version_id AND runtime.issue_id = gate.issue_id
    WHERE gate.question_id = ? AND gate.version_id = ? AND gate.issue_id = ?
      AND s.state = ? AND s.accepted_commit != ''
      AND runtime.state = ? AND runtime.delivered = 0
)`, questionID, versionID, issueID, SessionRunning, plan.StateNeedsAttention).Scan(&resumable); err != nil {
			return err
		}
		if resumable == 0 {
			return ErrNotFound
		}
	}
	if kind == "closed_unmerged_impact" {
		var decision closedPlanDecision
		if err := json.Unmarshal([]byte(answer), &decision); err != nil {
			return ErrInvalidClaim
		}
		switch decision.Action {
		case "cancel-plan":
			if decision.Replacement != nil {
				return ErrInvalidClaim
			}
			if err := s.cancelPlanTx(ctx, tx, versionID, now); err != nil {
				return err
			}
		case "replace":
			if decision.Replacement == nil {
				return ErrInvalidClaim
			}
			if err := replaceFrozenPlanTx(ctx, tx, repository, questionID, versionID, issueID, *decision.Replacement, now); err != nil {
				return err
			}
		default:
			return ErrInvalidClaim
		}
		if err := resolveFrozenPlanPollFailureTx(ctx, tx, repository, versionID, now); err != nil {
			return err
		}
	}
	if kind == "plan_amendment" {
		var decision amendmentDecision
		if err := json.Unmarshal([]byte(answer), &decision); err != nil {
			return ErrInvalidClaim
		}
		if err := s.resolvePlanAmendmentTx(ctx, tx, questionID, versionID, decision.Action, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = ?, answered_at = ? WHERE question_id = ?`, answer, formatTimestamp(now), questionID); err != nil {
		return err
	}
	if kind == "poll_failure" {
		if _, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors SET consecutive_failures = 0, failure_kind = '', recovery_state = '', recovery_plan_version_id = '', attempted_plan_version_ids_json = '[]', next_attempt_at = ?, updated_at = ? WHERE repository = ?`, formatTimestamp(now), formatTimestamp(now), repository); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT t.issue_id, COALESCE(s.session_id, ''), COALESCE(s.accepted_commit, '')
FROM poll_failure_targets t
LEFT JOIN ticket_sessions s ON s.version_id = t.version_id AND s.issue_id = t.issue_id
WHERE t.poll_question_id = ? AND t.version_id = ?`, questionID, versionID)
		if err != nil {
			return err
		}
		type recoveryTarget struct {
			issueID        int64
			sessionID      string
			acceptedCommit string
		}
		var targets []recoveryTarget
		for rows.Next() {
			var targetIssueID int64
			var sessionID, acceptedCommit string
			if err := rows.Scan(&targetIssueID, &sessionID, &acceptedCommit); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, recoveryTarget{issueID: targetIssueID, sessionID: sessionID, acceptedCommit: acceptedCommit})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, target := range targets {
			if err := recoverNeedsAttentionTicketTx(ctx, tx, versionID, target.issueID, target.sessionID, target.acceptedCommit, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = ?, answered_at = ? WHERE state = 'open' AND question_id IN (
    SELECT ticket_question_id FROM poll_failure_targets WHERE poll_question_id = ?
)`, "resolved by poll retry", formatTimestamp(now), questionID); err != nil {
			return err
		}
	} else if kind == "quality_gate" {
		var allowedJSON string
		err := tx.QueryRowContext(ctx, `SELECT allowed_answers_json FROM quality_gate_questions WHERE question_id = ? AND version_id = ? AND issue_id = ?`, questionID, versionID, issueID).Scan(&allowedJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var allowed []string
		if err := json.Unmarshal([]byte(allowedJSON), &allowed); err != nil || !slices.Contains(allowed, answer) {
			return ErrInvalidClaim
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET delivery_retry_pending = 1, updated_at = ? WHERE version_id = ? AND issue_id = ?`, formatTimestamp(now), versionID, issueID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(now), versionID, issueID); err != nil {
			return err
		}
	} else if kind == "needs_attention" {
		var sessionID, acceptedCommit string
		err := tx.QueryRowContext(ctx, `SELECT session_id, accepted_commit FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, versionID, issueID).Scan(&sessionID, &acceptedCommit)
		if errors.Is(err, sql.ErrNoRows) {
			sessionID = ""
			acceptedCommit = ""
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := recoverNeedsAttentionTicketTx(ctx, tx, versionID, issueID, sessionID, acceptedCommit, now); err != nil {
			return err
		}
	}
	return nil
}

func resolveFrozenPlanPollFailureTx(ctx context.Context, tx *sql.Tx, repository, versionID string, now time.Time) error {
	var pollQuestionID string
	err := tx.QueryRowContext(ctx, `SELECT question_id FROM workflow_questions
WHERE repository = ? AND version_id = ? AND issue_id = 0 AND kind = 'poll_failure' AND state = 'open'
ORDER BY generation DESC LIMIT 1`, repository, versionID).Scan(&pollQuestionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var attemptedJSON string
	var failureKind GitHubPollFailureKind
	var recoveryState GitHubPollRecoveryState
	var recoveryPlanVersionID string
	err = tx.QueryRowContext(ctx, `SELECT attempted_plan_version_ids_json, failure_kind, recovery_state, recovery_plan_version_id
FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&attemptedJSON, &failureKind, &recoveryState, &recoveryPlanVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var attemptedPlanVersionIDs []string
	if err := json.Unmarshal([]byte(attemptedJSON), &attemptedPlanVersionIDs); err != nil {
		return err
	}
	filtered := attemptedPlanVersionIDs[:0]
	retiredAttempted := false
	for _, attemptedVersionID := range attemptedPlanVersionIDs {
		if attemptedVersionID == versionID {
			retiredAttempted = true
			continue
		}
		filtered = append(filtered, attemptedVersionID)
	}
	terminalAffected := failureKind == GitHubPollFailureUnrecoverable && (retiredAttempted || recoveryPlanVersionID == versionID || pollQuestionID != "")
	if terminalAffected {
		if _, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET consecutive_failures = 0, failure_kind = '', recovery_state = '', recovery_plan_version_id = '', attempted_plan_version_ids_json = '[]', next_attempt_at = ?, updated_at = ?
WHERE repository = ?`, formatTimestamp(now), formatTimestamp(now), repository); err != nil {
			return err
		}
		if pollQuestionID == "" {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = ?, answered_at = ? WHERE question_id = ?`, "resolved by closed Plan decision", formatTimestamp(now), pollQuestionID)
		return err
	}
	if !retiredAttempted && recoveryPlanVersionID != versionID {
		return nil
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return err
	}
	if recoveryPlanVersionID == versionID {
		failureKind = GitHubPollFailureRetryable
		recoveryState = GitHubPollRecoveryConsumed
		recoveryPlanVersionID = ""
	}
	_, err = tx.ExecContext(ctx, `UPDATE github_poll_cursors
SET failure_kind = ?, recovery_state = ?, recovery_plan_version_id = ?, attempted_plan_version_ids_json = ?, updated_at = ?
WHERE repository = ?`, failureKind, recoveryState, recoveryPlanVersionID, string(encoded), formatTimestamp(now), repository)
	return err
}

func (s *Store) cancelPlanTx(ctx context.Context, tx *sql.Tx, versionID string, now time.Time) error {
	stamp := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND delivered = 0`, plan.StateCancelled, stamp, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = ? AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, stamp, RunRunning, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = ? AND session_id IN (SELECT session_id FROM ticket_sessions WHERE version_id = ?)`, LeaseActive, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET state = ?, owner = '', updated_at = ? WHERE version_id = ?`, SessionClosed, stamp, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_terminal_states(version_id, state, recorded_at) VALUES (?, ?, ?) ON CONFLICT(version_id) DO NOTHING`, versionID, "cancelled", stamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plan_freezes WHERE version_id = ?`, versionID); err != nil {
		return err
	}
	var repository string
	if err := tx.QueryRowContext(ctx, `SELECT p.repository FROM plans p JOIN plan_versions v ON v.plan_id = p.id WHERE v.version_id = ?`, versionID).Scan(&repository); err != nil {
		return err
	}
	_, err := s.queueWorkflowInboxProjectionTransitionTx(ctx, tx, repository, now)
	return err
}

func replaceFrozenPlanTx(ctx context.Context, tx *sql.Tx, repository, questionID, versionID string, questionIssueID int64, replacement replacementReference, now time.Time) error {
	var frozenIssueID int64
	if err := tx.QueryRowContext(ctx, `SELECT issue_id FROM plan_freezes WHERE version_id = ?`, versionID).Scan(&frozenIssueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		return err
	}
	if questionIssueID != frozenIssueID {
		return ErrNotReady
	}
	if (replacement.VersionID == "" || replacement.TicketID == 0) == (replacement.PlanRootIssueID == 0) {
		return ErrInvalidClaim
	}
	var replacementVersionID string
	var replacementIssueID int64
	if replacement.PlanRootIssueID != 0 {
		err := tx.QueryRowContext(ctx, `SELECT v.version_id
FROM plans p
JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE p.repository = ? AND p.root_issue_id = ? AND `+currentSchedulableReplacementPlanPredicate, repository, replacement.PlanRootIssueID).Scan(&replacementVersionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
	} else {
		err := tx.QueryRowContext(ctx, `SELECT v.version_id
FROM plan_tickets t
JOIN ticket_runtime r ON r.version_id = t.version_id AND r.issue_id = t.issue_id
JOIN plan_versions v ON v.version_id = t.version_id
JOIN plans p ON p.id = v.plan_id
WHERE t.version_id = ? AND t.issue_id = ? AND p.repository = ? AND r.delivered = 0 AND r.state != ? AND `+currentActiveUnfrozenPlanPredicate,
			replacement.VersionID, replacement.TicketID, repository, plan.StateCancelled).Scan(&replacementVersionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
		replacementIssueID = replacement.TicketID
	}
	if replacementVersionID == versionID {
		return ErrInvalidClaim
	}
	var reusesCancelledTicket int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1
    FROM plan_tickets source
    JOIN ticket_runtime runtime ON runtime.version_id = source.version_id AND runtime.issue_id = source.issue_id
    JOIN plan_tickets replacement ON replacement.issue_id = source.issue_id
    WHERE source.version_id = ? AND runtime.delivered = 0 AND replacement.version_id = ?
)`, versionID, replacementVersionID).Scan(&reusesCancelledTicket); err != nil {
		return err
	}
	if reusesCancelledTicket != 0 {
		return ErrInvalidClaim
	}
	replacementJSON, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	stamp := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO replacement_tickets(question_id, version_id, retired_issue_id, replacement, replacement_version_id, replacement_issue_id, state, approved_at)
VALUES (?, ?, ?, ?, ?, ?, 'approved', ?)`, questionID, versionID, frozenIssueID, string(replacementJSON), replacementVersionID, replacementIssueID, stamp); err != nil {
		return err
	}
	return nil
}

func (s *Store) SchedulerRoot(ctx context.Context, repository string, configuredRoot int64, now time.Time) (int64, error) {
	if repository == "" || configuredRoot <= 0 {
		return 0, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var sourceVersionID string
	err = tx.QueryRowContext(ctx, `SELECT p.current_version_id FROM plans p WHERE p.repository = ? AND p.root_issue_number = ?`, repository, configuredRoot).Scan(&sourceVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return configuredRoot, nil
	}
	if err != nil {
		return 0, err
	}
	var handoffState, targetVersionID string
	err = tx.QueryRowContext(ctx, `SELECT state, replacement_version_id FROM replacement_tickets
WHERE version_id = ? AND state IN ('approved', 'active') ORDER BY approved_at DESC LIMIT 1`, sourceVersionID).Scan(&handoffState, &targetVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return configuredRoot, nil
	}
	if err != nil {
		return 0, err
	}
	var targetRoot int64
	var targetVersionState, targetPlanState string
	err = tx.QueryRowContext(ctx, `SELECT p.root_issue_number,
COALESCE((SELECT terminal.state FROM plan_terminal_states terminal WHERE terminal.version_id = v.version_id), CASE WHEN EXISTS (SELECT 1 FROM completed_plan_versions completed WHERE completed.version_id = v.version_id) THEN ? ELSE v.state END), p.state
FROM plan_versions v JOIN plans p ON p.id = v.plan_id
WHERE v.version_id = ? AND p.repository = ? AND p.current_version_id = v.version_id`, StateCompleted, targetVersionID, repository).Scan(&targetRoot, &targetVersionState, &targetPlanState)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotReady
	}
	if err != nil {
		return 0, err
	}
	var frozen int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM plan_freezes WHERE version_id = ?`, targetVersionID).Scan(&frozen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	targetFrozen := err == nil
	if !targetFrozen && handoffState == "approved" && targetVersionState == StateProjecting && targetPlanState == StateProjecting {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return targetRoot, nil
	}
	targetActivated := targetPlanState == StateActive && (targetVersionState == StateActive || targetVersionState == StateCompleted)
	if targetFrozen || !targetActivated {
		return 0, ErrNotReady
	}
	if handoffState == "approved" {
		if err := s.cancelPlanTx(ctx, tx, sourceVersionID, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE replacement_tickets SET state = 'active' WHERE version_id = ? AND state = 'approved'`, sourceVersionID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return targetRoot, nil
}

func recoverNeedsAttentionTicketTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64, sessionID, acceptedCommit string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM plan_freezes WHERE version_id = ? AND issue_id = ?`, versionID, issueID); err != nil {
		return err
	}
	if acceptedCommit != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = 0, delivery_retry_pending = 1, updated_at = ? WHERE session_id = ?`, formatTimestamp(now), sessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(now), versionID, issueID)
		return err
	}
	if sessionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = 0, recovery_epoch = recovery_epoch + 1, updated_at = ? WHERE session_id = ?`, formatTimestamp(now), sessionID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateQueued, formatTimestamp(now), versionID, issueID)
	return err
}

func ensureWorkflowQuestionTx(ctx context.Context, tx *sql.Tx, repository, versionID string, issueID int64, kind, prompt string, now time.Time) error {
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = ? AND state = 'open'`, repository, versionID, issueID, kind).Scan(&open); err != nil {
		return err
	}
	if open != 0 {
		return nil
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = ?`, repository, versionID, issueID, kind).Scan(&generation); err != nil {
		return err
	}
	questionID := fmt.Sprintf("%s-%s-%d-g%d", strings.ReplaceAll(kind, "_", "-"), versionID, issueID, generation)
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, generation, prompt, state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?)
ON CONFLICT(repository, version_id, issue_id, kind, generation) DO NOTHING`, questionID, repository, versionID, issueID, kind, generation, prompt, formatTimestamp(now))
	return err
}

func markTicketNeedsAttentionTx(ctx context.Context, tx *sql.Tx, versionID string, issueID int64, reason string, now time.Time) error {
	var repository string
	var number int64
	err := tx.QueryRowContext(ctx, `SELECT p.repository, t.issue_number
FROM plan_tickets t
JOIN plan_versions v ON v.version_id = t.version_id
JOIN plans p ON p.id = v.plan_id
WHERE t.version_id = ? AND t.issue_id = ?`, versionID, issueID).Scan(&repository, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateNeedsAttention, formatTimestamp(now), versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET delivery_retry_pending = 0, updated_at = ? WHERE version_id = ? AND issue_id = ?`, formatTimestamp(now), versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id IN (
    SELECT run_id FROM worker_runs WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)
)`, RunRunning, versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE merge_ready_revalidations SET claimed_run_id = '' WHERE claimed_run_id IN (
    SELECT run_id FROM worker_runs WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)
)`, RunRunning, versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'cancelled', finished_at = ? WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)`, formatTimestamp(now), RunRunning, versionID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE state = ? AND session_id = (SELECT session_id FROM ticket_sessions WHERE version_id = ? AND issue_id = ?)`, LeaseActive, versionID, issueID); err != nil {
		return err
	}
	prompt := fmt.Sprintf("Ticket #%d needs attention: %s Reply with an id-addressed retry decision after resolving the cause.", number, reason)
	return ensureWorkflowQuestionTx(ctx, tx, repository, versionID, issueID, "needs_attention", prompt, now)
}

func markPlanNeedsAttentionTx(ctx context.Context, tx *sql.Tx, versionID, reason string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT issue_id FROM ticket_runtime WHERE version_id = ? AND delivered = 0`, versionID)
	if err != nil {
		return err
	}
	var issueIDs []int64
	for rows.Next() {
		var issueID int64
		if err := rows.Scan(&issueID); err != nil {
			rows.Close()
			return err
		}
		issueIDs = append(issueIDs, issueID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, issueID := range issueIDs {
		if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordGitHubPollFailure(ctx context.Context, repository string, now time.Time) error {
	_, err := s.AdvanceGitHubPollFailure(ctx, repository, now, "")
	return err
}

func (s *Store) RecordGitHubPollFailureWithKind(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind) error {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return err
	}
	recoveryPlanVersionID := ""
	if kind == GitHubPollFailurePreActivationInboxConflict {
		versionID, projecting, err := s.ProjectingDeliveryPlanVersion(ctx, repository)
		if err != nil {
			return errors.Join(err, s.releaseGitHubPollMutationLease(ctx, repository, token))
		}
		if projecting {
			recoveryPlanVersionID = versionID
		}
	}
	_, recordErr := s.AdvanceGitHubPollFailureLeased(ctx, repository, now, kind, recoveryPlanVersionID, token, leaseNow)
	return errors.Join(recordErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) AdvanceGitHubPollFailure(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind) (GitHubPollCursor, error) {
	token, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return GitHubPollCursor{}, err
	}
	cursor, recordErr := s.AdvanceGitHubPollFailureLeased(ctx, repository, now, kind, "", token, leaseNow)
	return cursor, errors.Join(recordErr, s.releaseGitHubPollMutationLease(ctx, repository, token))
}

func (s *Store) AdvanceGitHubPollFailureLeased(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind, recoveryPlanVersionID, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	return s.AdvanceGitHubPollFailureForPlanAttemptsLeased(ctx, repository, now, kind, recoveryPlanVersionID, nil, leaseToken, leaseNow)
}

func (s *Store) AdvanceGitHubPollFailureForPlanAttemptsLeased(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	return s.advanceGitHubPollFailureLeased(ctx, repository, now, kind, recoveryPlanVersionID, attemptedPlanVersionIDs, time.Time{}, leaseToken, leaseNow)
}

func (s *Store) advanceGitHubPollFailureLeased(ctx context.Context, repository string, now time.Time, kind GitHubPollFailureKind, recoveryPlanVersionID string, attemptedPlanVersionIDs []string, deferredUntil time.Time, leaseToken string, leaseNow time.Time) (GitHubPollCursor, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	deferredUntil = deferredUntil.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GitHubPollCursor{}, err
	}
	defer tx.Rollback()
	if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
		return GitHubPollCursor{}, err
	}
	var failures int
	var existingKind GitHubPollFailureKind
	var existingRecovery GitHubPollRecoveryState
	var existingRecoveryPlanVersionID string
	var existingAttemptedJSON, existingNextAttemptText string
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, attempted_plan_version_ids_json, next_attempt_at FROM github_poll_cursors WHERE repository = ?`, repository).Scan(&failures, &existingKind, &existingRecovery, &existingRecoveryPlanVersionID, &existingAttemptedJSON, &existingNextAttemptText)
	if errors.Is(err, sql.ErrNoRows) {
		failures = 0
	} else if err != nil {
		return GitHubPollCursor{}, err
	}
	previousFailures := failures
	failures++
	if previousFailures == 0 {
		existingAttemptedJSON = ""
	}
	attemptedJSON, err := mergeAttemptedPlanVersionIDs(existingAttemptedJSON, attemptedPlanVersionIDs)
	if err != nil {
		return GitHubPollCursor{}, err
	}
	recovery := GitHubPollRecoveryConsumed
	if existingKind == GitHubPollFailureUnrecoverable && existingRecovery == GitHubPollRecoveryConsumed {
		kind = GitHubPollFailureUnrecoverable
		recoveryPlanVersionID = existingRecoveryPlanVersionID
	} else if kind == GitHubPollFailurePreActivationInboxConflict && recoveryPlanVersionID != "" && (previousFailures == 0 || existingKind == GitHubPollFailureRetryable || existingKind == GitHubPollFailurePreActivationInboxConflict && (existingRecovery == GitHubPollRecoveryAvailable || existingRecovery == GitHubPollRecoveryClaimed) && existingRecoveryPlanVersionID == recoveryPlanVersionID) {
		recovery = GitHubPollRecoveryAvailable
		if existingKind == GitHubPollFailurePreActivationInboxConflict && existingRecovery == GitHubPollRecoveryClaimed && existingRecoveryPlanVersionID == recoveryPlanVersionID {
			recovery = GitHubPollRecoveryClaimed
		}
	} else {
		kind = GitHubPollFailureRetryable
	}
	delay := time.Second << min(failures-1, 6)
	nextAttempt := now.Add(delay)
	if !deferredUntil.IsZero() {
		nextAttempt = deferredUntil
		if existingNextAttemptText != "" {
			existingNextAttempt, parseErr := time.Parse(time.RFC3339Nano, existingNextAttemptText)
			if parseErr != nil {
				return GitHubPollCursor{}, parseErr
			}
			if existingNextAttempt.After(nextAttempt) {
				nextAttempt = existingNextAttempt
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO github_poll_cursors(repository, consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, attempted_plan_version_ids_json, next_attempt_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repository) DO UPDATE SET consecutive_failures = excluded.consecutive_failures, failure_kind = excluded.failure_kind, recovery_state = excluded.recovery_state, recovery_plan_version_id = excluded.recovery_plan_version_id, attempted_plan_version_ids_json = excluded.attempted_plan_version_ids_json, next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at`, repository, failures, kind, recovery, recoveryPlanVersionID, attemptedJSON, formatTimestamp(nextAttempt), formatTimestamp(now))
	if err != nil {
		return GitHubPollCursor{}, err
	}
	cursor, err := scanGitHubPollCursor(tx.QueryRowContext(ctx, `SELECT repository, last_success_at, last_full_reconcile_at, consecutive_failures, failure_kind, recovery_state, recovery_plan_version_id, next_attempt_at
FROM github_poll_cursors WHERE repository = ?`, repository))
	if err != nil {
		return GitHubPollCursor{}, err
	}
	if err := tx.Commit(); err != nil {
		return GitHubPollCursor{}, err
	}
	return cursor, nil
}

func mergeAttemptedPlanVersionIDs(existing string, additions []string) (string, error) {
	var versionIDs []string
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &versionIDs); err != nil {
			return "", err
		}
	}
	seen := make(map[string]struct{}, len(versionIDs)+len(additions))
	merged := make([]string, 0, len(versionIDs)+len(additions))
	for _, versionID := range append(versionIDs, additions...) {
		if versionID == "" {
			continue
		}
		if _, exists := seen[versionID]; exists {
			continue
		}
		seen[versionID] = struct{}{}
		merged = append(merged, versionID)
	}
	encoded, err := json.Marshal(merged)
	return string(encoded), err
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
