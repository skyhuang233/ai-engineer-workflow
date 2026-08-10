package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	candidateoutput "github.com/skyhuang233/workflow/internal/candidate"
	"github.com/skyhuang233/workflow/internal/plan"
)

var ErrSessionConflict = errors.New("ticket session identity conflict")

type TicketSession struct {
	SessionID              string
	VersionID              string
	TicketID               int64
	AgentIdentity          string
	CodexSessionID         string
	WorkspacePath          string
	CodexStatePath         string
	Branch                 string
	AcceptedCommit         string
	AcceptedCandidateRunID string
	WorkspaceReclaimedAt   time.Time
}

func (s *Store) ClosedSessionsForWorkspaceCleanup(ctx context.Context, retention time.Duration, now time.Time) ([]TicketSession, error) {
	if retention <= 0 {
		return nil, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, version_id, issue_id, workspace_path, codex_state_path
FROM ticket_sessions
WHERE state = ? AND workspace_reclaimed_at = '' AND updated_at <= ?
ORDER BY updated_at, session_id`, SessionClosed, formatTimestamp(now.Add(-retention)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []TicketSession
	for rows.Next() {
		var session TicketSession
		if err := rows.Scan(&session.SessionID, &session.VersionID, &session.TicketID, &session.WorkspacePath, &session.CodexStatePath); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) MarkWorkspaceReclaimed(ctx context.Context, sessionID string, now time.Time) error {
	if sessionID == "" {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ticket_sessions
SET workspace_reclaimed_at = ?
WHERE session_id = ? AND state = ? AND workspace_reclaimed_at = ''`, formatTimestamp(now), sessionID, SessionClosed)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrNotFound
	}
	return nil
}

type AgentBinding struct {
	SessionID      string
	AgentIdentity  string
	WorkspacePath  string
	CodexStatePath string
	Branch         string
}

type WorkerAudit struct {
	RunID                  string
	LeaseGeneration        int64
	ContainerID            string
	ImageDigest            string
	Mounts                 any
	ExtraHosts             any
	ToolVersions           any
	GitHubWriteCredentials bool
}

type CandidateRevision struct {
	RunID            string
	LeaseToken       string
	CodexSessionID   string
	CommitSHA        string
	StructuredOutput []byte
	ImageDigest      string
	ToolVersions     map[string]string
	Now              time.Time
	Publication      CandidatePublication
}

const defaultDeliveryLeaseTTL = 30 * time.Minute

type CandidatePublication struct {
	Repository         string
	Branch             string
	ExpectedRemoteHead string
	ExpectRemoteAbsent bool
	Title              string
	Body               string
}

type RunFailure struct {
	RunID           string
	LeaseToken      string
	DiagnosticsPath string
	Error           string
	Class           FailureClass
	Cause           error
	Now             time.Time
}

// FailureClass determines whether a replacement Worker Run may start
// immediately. Code and quality failures return to the same Ticket Agent;
// infrastructure failures wait behind a bounded exponential backoff.
type FailureClass string

const (
	FailureCodeQuality    FailureClass = "code_quality"
	FailureInfrastructure FailureClass = "infrastructure"
)

type WorkerAuditRecord struct {
	RunID                  string
	ContainerID            string
	ImageDigest            string
	MountsJSON             string
	ExtraHostsJSON         string
	ToolVersionsJSON       string
	GitHubWriteCredentials bool
}

type CandidateRecord struct {
	RunID            string
	SessionID        string
	CodexSessionID   string
	CommitSHA        string
	StructuredOutput []byte
}

type RecoveryRun struct {
	Claim   TicketClaim
	Kind    string
	Session TicketSession
}

// ExpiredAgentRecoveryRuns lists the current Agent Runs whose Run Leases have
// elapsed. Callers must isolate the corresponding container before using
// ReconcileMissingRecoveryRun to release the ticket for a replacement Run.
// The latter method repeats the current-run and Lease checks atomically, so a
// stale recovery observation can never supersede a newer generation.
func (s *Store) ExpiredAgentRecoveryRuns(ctx context.Context, versionID string, now time.Time) ([]RecoveryRun, error) {
	if versionID == "" {
		return nil, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.run_id, r.run_kind, s.version_id, s.issue_id, t.issue_number, t.title, s.owner, s.session_id,
s.agent_identity, s.codex_session_id, s.workspace_path, s.codex_state_path, s.branch, s.accepted_commit, s.accepted_candidate_run_id,
l.lease_token, l.generation, l.expires_at
FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.current_run_id = r.run_id AND r.run_kind = ? AND r.state = ? AND l.state = ? AND l.expires_at <= ?
ORDER BY r.started_at, r.run_id`, versionID, RunAgent, RunRunning, LeaseActive, formatTimestamp(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecoveryRuns(rows)
}

func (s *Store) ActiveRecoveryRuns(ctx context.Context, versionID string, now time.Time) ([]RecoveryRun, error) {
	if versionID == "" {
		return nil, ErrInvalidClaim
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
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT s.issue_id, r.run_id, l.lease_token
FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.current_run_id = r.run_id AND r.run_kind = ? AND r.state = ? AND l.state = ? AND l.expires_at <= ?`, versionID, RunDelivery, RunRunning, LeaseActive, formatTimestamp(now))
	if err != nil {
		return nil, err
	}
	type expiredDelivery struct {
		issueID           int64
		runID, leaseToken string
	}
	var expired []expiredDelivery
	for rows.Next() {
		var delivery expiredDelivery
		if err := rows.Scan(&delivery.issueID, &delivery.runID, &delivery.leaseToken); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, delivery)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, delivery := range expired {
		if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'failed', finished_at = ? WHERE run_id = ? AND state = ?`, formatTimestamp(now), delivery.runID, RunRunning); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND lease_token = ? AND state = ?`, delivery.runID, delivery.leaseToken, LeaseActive); err != nil {
			return nil, err
		}
		if err := markTicketNeedsAttentionTx(ctx, tx, versionID, delivery.issueID, "Delivery Controller lease expired during restart recovery", now); err != nil {
			return nil, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.run_id, r.run_kind, s.version_id, s.issue_id, t.issue_number, t.title, s.owner, s.session_id,
s.agent_identity, s.codex_session_id, s.workspace_path, s.codex_state_path, s.branch, s.accepted_commit, s.accepted_candidate_run_id,
l.lease_token, l.generation, l.expires_at
FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.current_run_id = r.run_id AND r.state = ? AND l.state = ? AND l.expires_at > ?
ORDER BY r.started_at, r.run_id`, versionID, RunRunning, LeaseActive, formatTimestamp(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs, err := scanRecoveryRuns(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return runs, nil
}

func scanRecoveryRuns(rows *sql.Rows) ([]RecoveryRun, error) {
	var runs []RecoveryRun
	for rows.Next() {
		var run RecoveryRun
		var expiresAt string
		if err := rows.Scan(&run.Claim.RunID, &run.Kind, &run.Claim.VersionID, &run.Claim.TicketID, &run.Claim.TicketNumber, &run.Claim.TicketTitle, &run.Claim.Owner, &run.Claim.SessionID,
			&run.Session.AgentIdentity, &run.Session.CodexSessionID, &run.Session.WorkspacePath, &run.Session.CodexStatePath, &run.Session.Branch, &run.Session.AcceptedCommit,
			&run.Session.AcceptedCandidateRunID,
			&run.Claim.LeaseToken, &run.Claim.LeaseGeneration, &expiresAt); err != nil {
			return nil, err
		}
		run.Session.SessionID = run.Claim.SessionID
		run.Session.VersionID = run.Claim.VersionID
		run.Session.TicketID = run.Claim.TicketID
		var parseErr error
		run.Claim.LeaseExpiresAt, parseErr = time.Parse(time.RFC3339Nano, expiresAt)
		if parseErr != nil {
			return nil, parseErr
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) ReconcileMissingRecoveryRun(ctx context.Context, run RecoveryRun, reason string, now time.Time) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if run.Claim.RunID == "" || run.Claim.LeaseToken == "" || run.Claim.LeaseGeneration <= 0 || strings.TrimSpace(reason) == "" {
		return ErrInvalidClaim
	}
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
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT r.run_kind FROM worker_runs r
JOIN ticket_sessions s ON s.current_run_id = r.run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND l.lease_token = ? AND l.generation = ? AND r.state = ? AND l.state = ?`, run.Claim.RunID, run.Claim.LeaseToken, run.Claim.LeaseGeneration, RunRunning, LeaseActive).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	if kind == RunDelivery {
		if err := markTicketNeedsAttentionTx(ctx, tx, run.Claim.VersionID, run.Claim.TicketID, "Delivery Controller was not recoverable after restart: "+reason, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'superseded', finished_at = ? WHERE run_id = ? AND state = ?`, formatTimestamp(now), run.Claim.RunID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND lease_token = ? AND state = ?`, run.Claim.RunID, run.Claim.LeaseToken, LeaseActive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, run.Claim.RunID); err != nil {
		return err
	}
	if _, err := releaseMergeReadyRevalidationsTx(ctx, tx, run.Claim.RunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateQueued, formatTimestamp(now), run.Claim.VersionID, run.Claim.TicketID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TicketSession(ctx context.Context, versionID string, ticketID int64) (TicketSession, error) {
	var session TicketSession
	err := s.db.QueryRowContext(ctx, `SELECT session_id, version_id, issue_id, agent_identity, codex_session_id, workspace_path, codex_state_path, branch, accepted_commit, accepted_candidate_run_id
FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, versionID, ticketID).Scan(
		&session.SessionID, &session.VersionID, &session.TicketID, &session.AgentIdentity,
		&session.CodexSessionID, &session.WorkspacePath, &session.CodexStatePath,
		&session.Branch, &session.AcceptedCommit, &session.AcceptedCandidateRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketSession{}, ErrNotFound
	}
	return session, err
}

func (s *Store) WorkspaceForRun(ctx context.Context, runID string) (string, error) {
	var workspace string
	err := s.db.QueryRowContext(ctx, `SELECT s.workspace_path FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?`, runID).Scan(&workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if workspace == "" {
		return "", ErrNotFound
	}
	return workspace, nil
}

func (s *Store) BindAgent(ctx context.Context, binding AgentBinding) (TicketSession, error) {
	if binding.SessionID == "" || binding.AgentIdentity == "" || binding.WorkspacePath == "" || binding.CodexStatePath == "" || binding.Branch == "" {
		return TicketSession{}, ErrInvalidClaim
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketSession{}, err
	}
	defer tx.Rollback()
	var current AgentBinding
	err = tx.QueryRowContext(ctx, `SELECT session_id, agent_identity, workspace_path, codex_state_path, branch FROM ticket_sessions WHERE session_id = ?`, binding.SessionID).
		Scan(&current.SessionID, &current.AgentIdentity, &current.WorkspacePath, &current.CodexStatePath, &current.Branch)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketSession{}, ErrNotFound
	}
	if err != nil {
		return TicketSession{}, err
	}
	if (current.AgentIdentity != "" && current.AgentIdentity != binding.AgentIdentity) ||
		(current.WorkspacePath != "" && current.WorkspacePath != binding.WorkspacePath) ||
		(current.CodexStatePath != "" && current.CodexStatePath != binding.CodexStatePath) ||
		(current.Branch != "" && current.Branch != binding.Branch) {
		return TicketSession{}, ErrSessionConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE ticket_sessions SET agent_identity = ?, workspace_path = ?, codex_state_path = ?, branch = ?, updated_at = ? WHERE session_id = ?`,
		binding.AgentIdentity, binding.WorkspacePath, binding.CodexStatePath, binding.Branch, formatTimestamp(time.Now()), binding.SessionID)
	if err != nil {
		return TicketSession{}, err
	}
	var session TicketSession
	err = tx.QueryRowContext(ctx, `SELECT session_id, version_id, issue_id, agent_identity, codex_session_id, workspace_path, codex_state_path, branch, accepted_commit, accepted_candidate_run_id FROM ticket_sessions WHERE session_id = ?`, binding.SessionID).Scan(
		&session.SessionID, &session.VersionID, &session.TicketID, &session.AgentIdentity, &session.CodexSessionID,
		&session.WorkspacePath, &session.CodexStatePath, &session.Branch, &session.AcceptedCommit, &session.AcceptedCandidateRunID)
	if err != nil {
		return TicketSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketSession{}, err
	}
	return session, nil
}

func (s *Store) RecordWorkerAudit(ctx context.Context, audit WorkerAudit) error {
	if audit.RunID == "" || audit.LeaseGeneration <= 0 || audit.ImageDigest == "" {
		return ErrInvalidClaim
	}
	mounts, err := json.Marshal(audit.Mounts)
	if err != nil {
		return err
	}
	versions, err := json.Marshal(audit.ToolVersions)
	if err != nil {
		return err
	}
	extraHosts, err := json.Marshal(audit.ExtraHosts)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO worker_audits(run_id, container_id, image_digest, mounts_json, extra_hosts_json, tool_versions_json, github_write_credentials, created_at)
SELECT r.run_id, ?, ?, ?, ?, ?, ?, ? FROM worker_runs r
WHERE r.run_id = ? AND r.lease_generation = ?`, audit.ContainerID, audit.ImageDigest, string(mounts), string(extraHosts), string(versions), boolInt(audit.GitHubWriteCredentials), formatTimestamp(time.Now()), audit.RunID, audit.LeaseGeneration)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrInvalidClaim
	}
	return nil
}

func (s *Store) WorkerAudit(ctx context.Context, runID string) (WorkerAuditRecord, error) {
	var record WorkerAuditRecord
	var credentials int
	err := s.db.QueryRowContext(ctx, `SELECT run_id, container_id, image_digest, mounts_json, extra_hosts_json, tool_versions_json, github_write_credentials FROM worker_audits WHERE run_id = ?`, runID).
		Scan(&record.RunID, &record.ContainerID, &record.ImageDigest, &record.MountsJSON, &record.ExtraHostsJSON, &record.ToolVersionsJSON, &credentials)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerAuditRecord{}, ErrNotFound
	}
	record.GitHubWriteCredentials = credentials != 0
	return record, err
}

func (s *Store) RecordCodexSession(ctx context.Context, runID, leaseToken, codexSessionID string) error {
	if runID == "" || leaseToken == "" || codexSessionID == "" {
		return ErrInvalidClaim
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID, existing string
	err = tx.QueryRowContext(ctx, `SELECT r.session_id, s.codex_session_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND s.current_run_id = r.run_id AND r.run_kind = ? AND l.lease_token = ? AND r.state = ? AND l.state = ? AND l.expires_at > ?`, runID, RunAgent, leaseToken, RunRunning, LeaseActive, formatTimestamp(now)).Scan(&sessionID, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	if existing != "" && existing != codexSessionID {
		return ErrSessionConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET codex_session_id = ?, updated_at = ? WHERE session_id = ?`, codexSessionID, formatTimestamp(now), sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RunDiagnostic(ctx context.Context, runID string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT diagnostics_path FROM run_diagnostics WHERE run_id = ?`, runID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return path, err
}

func (s *Store) CandidateRevision(ctx context.Context, runID string) (CandidateRecord, error) {
	var record CandidateRecord
	var structured string
	err := s.db.QueryRowContext(ctx, `SELECT run_id, session_id, codex_session_id, commit_sha, structured_output FROM candidate_revisions WHERE run_id = ?`, runID).
		Scan(&record.RunID, &record.SessionID, &record.CodexSessionID, &record.CommitSHA, &structured)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateRecord{}, ErrNotFound
	}
	record.StructuredOutput = []byte(structured)
	return record, err
}

func (s *Store) AcceptCandidate(ctx context.Context, candidate CandidateRevision) error {
	_, err := s.acceptCandidate(ctx, candidate, 0)
	return err
}

func (s *Store) AcceptCandidateForDelivery(ctx context.Context, candidate CandidateRevision, deliveryLeaseTTL time.Duration) (TicketClaim, error) {
	if deliveryLeaseTTL <= 0 {
		return TicketClaim{}, ErrInvalidClaim
	}
	return s.acceptCandidate(ctx, candidate, deliveryLeaseTTL)
}

func (s *Store) CandidateWorkerRuntime(ctx context.Context, versionID string, issueID int64) (string, map[string]string, error) {
	var imageDigest, toolVersionsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT c.image_digest, c.tool_versions_json
FROM ticket_sessions s
JOIN candidate_revisions c ON c.run_id = s.accepted_candidate_run_id
WHERE s.version_id = ? AND s.issue_id = ?`, versionID, issueID).
		Scan(&imageDigest, &toolVersionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	var toolVersions map[string]string
	if imageDigest == "" || json.Unmarshal([]byte(toolVersionsJSON), &toolVersions) != nil || toolVersions["codex"] == "" || toolVersions["no-mistakes"] == "" {
		return "", nil, ErrInvalidClaim
	}
	return imageDigest, toolVersions, nil
}

func (s *Store) acceptCandidate(ctx context.Context, candidate CandidateRevision, deliveryLeaseTTL time.Duration) (TicketClaim, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if candidate.RunID == "" || candidate.LeaseToken == "" || candidate.CodexSessionID == "" || candidate.CommitSHA == "" || len(candidate.StructuredOutput) == 0 || candidate.Publication.Repository == "" || candidate.Publication.Branch == "" || candidate.Publication.Title == "" || (candidate.Publication.ExpectedRemoteHead == "") == !candidate.Publication.ExpectRemoteAbsent {
		return TicketClaim{}, ErrInvalidClaim
	}
	if candidateoutput.Validate(candidate.StructuredOutput) != nil {
		return TicketClaim{}, ErrInvalidClaim
	}
	toolVersions, err := json.Marshal(candidate.ToolVersions)
	if err != nil {
		return TicketClaim{}, err
	}
	if candidate.Now.IsZero() {
		candidate.Now = time.Now().UTC()
	} else {
		candidate.Now = candidate.Now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketClaim{}, err
	}
	defer tx.Rollback()
	var sessionID, currentSessionID, existingCodexSessionID string
	err = tx.QueryRowContext(ctx, `SELECT r.session_id, s.session_id, s.codex_session_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
	WHERE r.run_id = ? AND s.current_run_id = r.run_id AND r.run_kind = ? AND l.lease_token = ? AND r.state = ? AND l.state = ? AND l.expires_at > ?`, candidate.RunID, RunAgent, candidate.LeaseToken, RunRunning, LeaseActive, formatTimestamp(candidate.Now)).Scan(&sessionID, &currentSessionID, &existingCodexSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, ErrInvalidClaim
	}
	if err != nil {
		return TicketClaim{}, err
	}
	if sessionID != currentSessionID {
		return TicketClaim{}, ErrInvalidClaim
	}
	if existingCodexSessionID != "" && existingCodexSessionID != candidate.CodexSessionID {
		return TicketClaim{}, ErrSessionConflict
	}
	now := formatTimestamp(candidate.Now)
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_outbox
SET state = ?, last_error = ?, claim_token = '', completed_at = ?, updated_at = ?
	WHERE json_extract(request_json, '$.run_id') = ? AND state IN (?, ?)`, OutboxRejected, "candidate accepted before delivery controller admission", now, now, candidate.RunID, OutboxPending, OutboxProcessing); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO candidate_revisions(run_id, session_id, codex_session_id, commit_sha, structured_output, image_digest, tool_versions_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, candidate.RunID, sessionID, candidate.CodexSessionID, candidate.CommitSHA, string(candidate.StructuredOutput), candidate.ImageDigest, string(toolVersions), now); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded', finished_at = ? WHERE run_id = ? AND state = ?`, now, candidate.RunID, RunRunning); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, candidate.RunID, candidate.LeaseToken, LeaseActive); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET codex_session_id = CASE WHEN codex_session_id = '' THEN ? ELSE codex_session_id END, accepted_commit = ?, accepted_candidate_run_id = ?, consecutive_failures = 0, recovery_epoch = recovery_epoch + 1, updated_at = ? WHERE session_id = ? AND (codex_session_id = '' OR codex_session_id = ?)`, candidate.CodexSessionID, candidate.CommitSHA, candidate.RunID, now, sessionID, candidate.CodexSessionID); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM infrastructure_retry_backoffs WHERE session_id = ?`, sessionID); err != nil {
		return TicketClaim{}, err
	}
	remoteHead := candidate.Publication.ExpectedRemoteHead
	if candidate.Publication.ExpectRemoteAbsent {
		remoteHead = ""
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_deliveries(version_id, issue_id, repository, branch, remote_head, created_at, updated_at)
SELECT version_id, issue_id, ?, ?, ?, ?, ? FROM ticket_sessions WHERE session_id = ?
ON CONFLICT(version_id, issue_id) DO UPDATE SET repository = excluded.repository, branch = excluded.branch, remote_head = excluded.remote_head,
validated_base_commit = '', validated_head_commit = '', checks_etag = '', updated_at = excluded.updated_at`, candidate.Publication.Repository, candidate.Publication.Branch, remoteHead, now, now, sessionID); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = (SELECT version_id FROM ticket_sessions WHERE session_id = ?) AND issue_id = (SELECT issue_id FROM ticket_sessions WHERE session_id = ?) AND delivered = 0`, plan.StateWaitingReview, now, sessionID, sessionID); err != nil {
		return TicketClaim{}, err
	}
	var delivery TicketClaim
	if deliveryLeaseTTL > 0 {
		if delivery, err = claimDeliveryControllerTx(ctx, tx, sessionID, deliveryLeaseTTL, candidate.Now); err != nil {
			return TicketClaim{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return TicketClaim{}, err
	}
	return delivery, nil
}

func claimDeliveryControllerTx(ctx context.Context, tx *sql.Tx, sessionID string, leaseTTL time.Duration, now time.Time) (TicketClaim, error) {
	var claim TicketClaim
	var generation, recoveryEpoch int64
	err := tx.QueryRowContext(ctx, `SELECT s.version_id, s.issue_id, s.owner, s.current_lease_generation, s.recovery_epoch, t.issue_number, t.title
FROM ticket_sessions s JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
WHERE s.session_id = ? AND s.state = ?`, sessionID, SessionRunning).Scan(&claim.VersionID, &claim.TicketID, &claim.Owner, &generation, &recoveryEpoch, &claim.TicketNumber, &claim.TicketTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketClaim{}, ErrInvalidClaim
	}
	if err != nil {
		return TicketClaim{}, err
	}
	claim.SessionID = sessionID
	generation++
	runID, err := randomID("run-")
	if err != nil {
		return TicketClaim{}, err
	}
	leaseToken, err := randomID("lease-")
	if err != nil {
		return TicketClaim{}, err
	}
	expiresAt := now.Add(leaseTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_runs(run_id, session_id, attempt, recovery_epoch, lease_generation, state, started_at, run_kind) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, runID, sessionID, -int(generation), recoveryEpoch, generation, RunRunning, formatTimestamp(now), RunDelivery); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_leases(lease_token, run_id, session_id, generation, state, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, leaseToken, runID, sessionID, generation, LeaseActive, formatTimestamp(expiresAt), formatTimestamp(now)); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET current_run_id = ?, current_lease_generation = ?, updated_at = ? WHERE session_id = ?`, runID, generation, formatTimestamp(now), sessionID); err != nil {
		return TicketClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateRunning, formatTimestamp(now), claim.VersionID, claim.TicketID); err != nil {
		return TicketClaim{}, err
	}
	claim.RunID = runID
	claim.Attempt = -int(generation)
	claim.LeaseToken = leaseToken
	claim.LeaseGeneration = generation
	claim.LeaseExpiresAt = expiresAt
	return claim, nil
}

func (s *Store) CompleteDeliveryController(ctx context.Context, claim TicketClaim, now time.Time) error {
	return s.finishDeliveryController(ctx, claim, "", FailureCodeQuality, false, now)
}

func (s *Store) FailDeliveryController(ctx context.Context, claim TicketClaim, reason string, now time.Time) error {
	return s.finishDeliveryController(ctx, claim, reason, FailureCodeQuality, false, now)
}

// FailDeliveryControllerWithClass releases the failed Delivery Controller and
// schedules a fenced retry of its accepted Candidate Revision. The bounded
// retry count is per recovery epoch; escalation remains the explicit fallback.
func (s *Store) FailDeliveryControllerWithClass(ctx context.Context, claim TicketClaim, reason string, class FailureClass, now time.Time) error {
	return s.finishDeliveryController(ctx, claim, reason, class, true, now)
}

func (s *Store) finishDeliveryController(ctx context.Context, claim TicketClaim, reason string, class FailureClass, retry bool, now time.Time) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if claim.VersionID == "" || claim.TicketID == 0 || claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if class == "" {
		class = FailureCodeQuality
	}
	if class != FailureCodeQuality && class != FailureInfrastructure {
		return ErrInvalidClaim
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID, expiresText string
	err = tx.QueryRowContext(ctx, `SELECT s.session_id, l.expires_at
FROM ticket_sessions s JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE s.version_id = ? AND s.issue_id = ? AND r.run_id = ? AND r.run_kind = ? AND r.state = ? AND l.lease_token = ? AND l.generation = ? AND l.state = ?`, claim.VersionID, claim.TicketID, claim.RunID, RunDelivery, RunRunning, claim.LeaseToken, claim.LeaseGeneration, LeaseActive).Scan(&sessionID, &expiresText)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return err
	}
	if !expiresAt.After(now) {
		nowText := formatTimestamp(now)
		if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'failed', finished_at = ? WHERE run_id = ? AND state = ?`, nowText, claim.RunID, RunRunning); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND lease_token = ? AND state = ?`, claim.RunID, claim.LeaseToken, LeaseActive); err != nil {
			return err
		}
		if err := markTicketNeedsAttentionTx(ctx, tx, claim.VersionID, claim.TicketID, "Delivery Controller lease expired before completion", now); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrNeedsAttention
	}
	state := "succeeded"
	if reason != "" {
		state = "failed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = ?, finished_at = ? WHERE run_id = ? AND state = ?`, state, formatTimestamp(now), claim.RunID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, claim.RunID, claim.LeaseToken, LeaseActive); err != nil {
		return err
	}
	if reason == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(now), claim.VersionID, claim.TicketID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE quality_gate_questions SET consumed_at = ? WHERE session_id = ? AND consumed_at = '' AND question_id IN (
    SELECT question_id FROM workflow_questions WHERE state = 'answered'
)`, formatTimestamp(now), sessionID); err != nil {
			return err
		}
	} else if retry {
		if err := recordRunFailureDetailTx(ctx, tx, claim.RunID, class, reason, now); err != nil {
			return err
		}
		if err := updateInfrastructureRetryTx(ctx, tx, claim.RunID, class, now); err != nil {
			return err
		}
		var recoveryEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT recovery_epoch FROM ticket_sessions WHERE session_id = ?`, sessionID).Scan(&recoveryEpoch); err != nil {
			return err
		}
		var attempts int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_runs WHERE session_id = ? AND recovery_epoch = ? AND run_kind = ?`, sessionID, recoveryEpoch, RunDelivery).Scan(&attempts); err != nil {
			return err
		}
		if attempts < DefaultMaxWorkerAttempts {
			if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET delivery_retry_pending = 1, updated_at = ? WHERE session_id = ?`, formatTimestamp(now), sessionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateWaitingReview, formatTimestamp(now), claim.VersionID, claim.TicketID); err != nil {
				return err
			}
		} else if err := markTicketNeedsAttentionTx(ctx, tx, claim.VersionID, claim.TicketID, fmt.Sprintf("Delivery Controller retry budget exhausted after %d attempts: %s", attempts, reason), now); err != nil {
			return err
		}
	} else if err := markTicketNeedsAttentionTx(ctx, tx, claim.VersionID, claim.TicketID, "Delivery Controller failed: "+reason, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordRunFailure(ctx context.Context, failure RunFailure) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if failure.RunID == "" || failure.LeaseToken == "" {
		return ErrInvalidClaim
	}
	if failure.Now.IsZero() {
		failure.Now = time.Now().UTC()
	} else {
		failure.Now = failure.Now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var authenticationFailure *SessionAuthenticationFailure
	isAuthenticationFailure := errors.As(failure.Cause, &authenticationFailure)
	if isAuthenticationFailure {
		failure.Error = ErrSessionAuthenticationUnavailable.Error()
	}
	if failure.Class == "" {
		failure.Class = FailureCodeQuality
	}
	if failure.Class != FailureCodeQuality && failure.Class != FailureInfrastructure {
		return ErrInvalidClaim
	}
	var currentRunID, runState, leaseState, expiresText, versionID string
	var issueID int64
	err = tx.QueryRowContext(ctx, `SELECT s.current_run_id, r.state, l.state, l.expires_at, s.version_id, s.issue_id
FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND l.lease_token = ? AND r.run_kind = ?`, failure.RunID, failure.LeaseToken, RunAgent).Scan(&currentRunID, &runState, &leaseState, &expiresText, &versionID, &issueID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	now := formatTimestamp(failure.Now)
	if failure.DiagnosticsPath != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_diagnostics(run_id, diagnostics_path, error, created_at) VALUES (?, ?, ?, ?) ON CONFLICT(run_id) DO NOTHING`, failure.RunID, failure.DiagnosticsPath, failure.Error, now); err != nil {
			return err
		}
	}
	if currentRunID != failure.RunID || runState != RunRunning || leaseState != LeaseActive {
		return tx.Commit()
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return err
	}
	if !expiresAt.After(failure.Now) && !isAuthenticationFailure {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'failed', finished_at = ? WHERE run_id = ? AND state = ?`, now, failure.RunID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, failure.RunID, failure.LeaseToken, LeaseActive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET consecutive_failures = consecutive_failures + 1, updated_at = ? WHERE session_id = (SELECT session_id FROM worker_runs WHERE run_id = ?)`, now, failure.RunID); err != nil {
		return err
	}
	if err := recordRunFailureDetailTx(ctx, tx, failure.RunID, failure.Class, failure.Error, failure.Now); err != nil {
		return err
	}
	if err := updateInfrastructureRetryTx(ctx, tx, failure.RunID, failure.Class, failure.Now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, failure.RunID)
	if err != nil {
		return err
	}
	claimedFeedback, err := result.RowsAffected()
	if err != nil {
		return err
	}
	claimedRevalidations, err := releaseMergeReadyRevalidationsTx(ctx, tx, failure.RunID)
	if err != nil {
		return err
	}
	claimedFeedback += claimedRevalidations
	if isAuthenticationFailure {
		if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, ErrSessionAuthenticationUnavailable.Error(), failure.Now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if claimedFeedback > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ?
WHERE (version_id, issue_id) = (SELECT s.version_id, s.issue_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?) AND state = ? AND delivered = 0`, plan.StateWaitingReview, now, failure.RunID, plan.StateRunning); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recordRunFailureDetailTx(ctx context.Context, tx *sql.Tx, runID string, class FailureClass, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO run_failures(run_id, class, reason, recorded_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(run_id) DO NOTHING`, runID, class, reason, formatTimestamp(now))
	return err
}

func updateInfrastructureRetryTx(ctx context.Context, tx *sql.Tx, runID string, class FailureClass, now time.Time) error {
	var sessionID string
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM worker_runs WHERE run_id = ?`, runID).Scan(&sessionID); err != nil {
		return err
	}
	if class != FailureInfrastructure {
		_, err := tx.ExecContext(ctx, `DELETE FROM infrastructure_retry_backoffs WHERE session_id = ?`, sessionID)
		return err
	}
	var failures int
	err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM infrastructure_retry_backoffs WHERE session_id = ?`, sessionID).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		failures = 0
	} else if err != nil {
		return err
	}
	failures++
	_, err = tx.ExecContext(ctx, `INSERT INTO infrastructure_retry_backoffs(session_id, consecutive_failures, retry_at)
VALUES (?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET consecutive_failures = excluded.consecutive_failures, retry_at = excluded.retry_at`,
		sessionID, failures, formatTimestamp(now.Add(infrastructureRetryDelay(failures))))
	return err
}

func infrastructureRetryDelay(failures int) time.Duration {
	if failures <= 1 {
		return time.Minute
	}
	if failures >= 6 {
		return 30 * time.Minute
	}
	return time.Minute * time.Duration(1<<(failures-1))
}
