package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

var ErrSessionConflict = errors.New("ticket session identity conflict")

type TicketSession struct {
	SessionID      string
	VersionID      string
	TicketID       int64
	AgentIdentity  string
	CodexSessionID string
	WorkspacePath  string
	CodexStatePath string
	Branch         string
	AcceptedCommit string
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
	LeaseToken             string
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
	Now              time.Time
	Publication      CandidatePublication
}

type CandidatePublication struct {
	Repository         string
	Branch             string
	ExpectedRemoteHead string
	ExpectRemoteAbsent bool
	Title              string
	Body               string
}

type CandidateHandoff struct {
	PushOutboxKey string
	PROutboxKey   string
}

type RunFailure struct {
	RunID           string
	LeaseToken      string
	DiagnosticsPath string
	Error           string
	Now             time.Time
}

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

func (s *Store) TicketSession(ctx context.Context, versionID string, ticketID int64) (TicketSession, error) {
	var session TicketSession
	err := s.db.QueryRowContext(ctx, `SELECT session_id, version_id, issue_id, agent_identity, codex_session_id, workspace_path, codex_state_path, branch, accepted_commit
FROM ticket_sessions WHERE version_id = ? AND issue_id = ?`, versionID, ticketID).Scan(
		&session.SessionID, &session.VersionID, &session.TicketID, &session.AgentIdentity,
		&session.CodexSessionID, &session.WorkspacePath, &session.CodexStatePath,
		&session.Branch, &session.AcceptedCommit)
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
	err = tx.QueryRowContext(ctx, `SELECT session_id, version_id, issue_id, agent_identity, codex_session_id, workspace_path, codex_state_path, branch, accepted_commit FROM ticket_sessions WHERE session_id = ?`, binding.SessionID).Scan(
		&session.SessionID, &session.VersionID, &session.TicketID, &session.AgentIdentity, &session.CodexSessionID,
		&session.WorkspacePath, &session.CodexStatePath, &session.Branch, &session.AcceptedCommit)
	if err != nil {
		return TicketSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return TicketSession{}, err
	}
	return session, nil
}

func (s *Store) RecordWorkerAudit(ctx context.Context, audit WorkerAudit) error {
	if audit.RunID == "" || audit.LeaseToken == "" || audit.ImageDigest == "" {
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
SELECT r.run_id, ?, ?, ?, ?, ?, ?, ? FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND l.lease_token = ? AND r.state = ? AND l.state = ?`, audit.ContainerID, audit.ImageDigest, string(mounts), string(extraHosts), string(versions), boolInt(audit.GitHubWriteCredentials), formatTimestamp(time.Now()), audit.RunID, audit.LeaseToken, RunRunning, LeaseActive)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID, existing string
	err = tx.QueryRowContext(ctx, `SELECT r.session_id, s.codex_session_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND l.lease_token = ? AND r.state = ? AND l.state = ?`, runID, leaseToken, RunRunning, LeaseActive).Scan(&sessionID, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	if existing != "" && existing != codexSessionID {
		return ErrSessionConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET codex_session_id = ?, updated_at = ? WHERE session_id = ?`, codexSessionID, formatTimestamp(time.Now()), sessionID); err != nil {
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

func (s *Store) AcceptedCandidateHandoff(ctx context.Context, versionID string, ticketID int64) (CandidateRecord, CandidateHandoff, error) {
	var candidate CandidateRecord
	var structured string
	err := s.db.QueryRowContext(ctx, `SELECT c.run_id, c.session_id, c.codex_session_id, c.commit_sha, c.structured_output
FROM candidate_revisions c
JOIN ticket_sessions s ON s.session_id = c.session_id
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
JOIN worker_runs r ON r.run_id = c.run_id
WHERE s.version_id = ? AND s.issue_id = ? AND rt.state = ? AND r.state = 'succeeded'`, versionID, ticketID, plan.StateWaitingReview).
		Scan(&candidate.RunID, &candidate.SessionID, &candidate.CodexSessionID, &candidate.CommitSHA, &structured)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateRecord{}, CandidateHandoff{}, ErrNotFound
	}
	if err != nil {
		return CandidateRecord{}, CandidateHandoff{}, err
	}
	candidate.StructuredOutput = []byte(structured)
	rows, err := s.db.QueryContext(ctx, `SELECT o.operation, o.idempotency_key
FROM accepted_candidate_outbox a
JOIN delivery_outbox o ON o.idempotency_key = a.outbox_key
WHERE a.run_id = ? AND o.operation IN (?, ?)`, candidate.RunID, DeliveryPushCandidate, DeliveryUpsertPR)
	if err != nil {
		return CandidateRecord{}, CandidateHandoff{}, err
	}
	defer rows.Close()
	var handoff CandidateHandoff
	for rows.Next() {
		var operation DeliveryOperation
		var key string
		if err := rows.Scan(&operation, &key); err != nil {
			return CandidateRecord{}, CandidateHandoff{}, err
		}
		switch operation {
		case DeliveryPushCandidate:
			handoff.PushOutboxKey = key
		case DeliveryUpsertPR:
			handoff.PROutboxKey = key
		}
	}
	if err := rows.Err(); err != nil {
		return CandidateRecord{}, CandidateHandoff{}, err
	}
	if handoff.PushOutboxKey == "" || handoff.PROutboxKey == "" {
		return CandidateRecord{}, CandidateHandoff{}, ErrNotFound
	}
	return candidate, handoff, nil
}

func (s *Store) AcceptCandidate(ctx context.Context, candidate CandidateRevision) (CandidateHandoff, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if candidate.RunID == "" || candidate.LeaseToken == "" || candidate.CodexSessionID == "" || candidate.CommitSHA == "" || len(candidate.StructuredOutput) == 0 || candidate.Publication.Repository == "" || candidate.Publication.Branch == "" || candidate.Publication.Title == "" || (candidate.Publication.ExpectedRemoteHead == "") == !candidate.Publication.ExpectRemoteAbsent {
		return CandidateHandoff{}, ErrInvalidClaim
	}
	if !json.Valid(candidate.StructuredOutput) {
		return CandidateHandoff{}, ErrInvalidClaim
	}
	if candidate.Now.IsZero() {
		candidate.Now = time.Now().UTC()
	} else {
		candidate.Now = candidate.Now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CandidateHandoff{}, err
	}
	defer tx.Rollback()
	var sessionID, currentSessionID, existingCodexSessionID string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT r.session_id, s.session_id, s.codex_session_id, l.generation FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
	WHERE r.run_id = ? AND l.lease_token = ? AND r.state = ? AND l.state = ? AND l.expires_at > ?`, candidate.RunID, candidate.LeaseToken, RunRunning, LeaseActive, formatTimestamp(candidate.Now)).Scan(&sessionID, &currentSessionID, &existingCodexSessionID, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateHandoff{}, ErrInvalidClaim
	}
	if err != nil {
		return CandidateHandoff{}, err
	}
	if sessionID != currentSessionID || generation <= 0 {
		return CandidateHandoff{}, ErrInvalidClaim
	}
	if existingCodexSessionID != "" && existingCodexSessionID != candidate.CodexSessionID {
		return CandidateHandoff{}, ErrSessionConflict
	}
	now := formatTimestamp(candidate.Now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO candidate_revisions(run_id, session_id, codex_session_id, commit_sha, structured_output, created_at) VALUES (?, ?, ?, ?, ?, ?)`, candidate.RunID, sessionID, candidate.CodexSessionID, candidate.CommitSHA, string(candidate.StructuredOutput), now); err != nil {
		return CandidateHandoff{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded', finished_at = ? WHERE run_id = ? AND state = ?`, now, candidate.RunID, RunRunning); err != nil {
		return CandidateHandoff{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, candidate.RunID, candidate.LeaseToken, LeaseActive); err != nil {
		return CandidateHandoff{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET codex_session_id = CASE WHEN codex_session_id = '' THEN ? ELSE codex_session_id END, accepted_commit = ?, consecutive_failures = 0, updated_at = ? WHERE session_id = ? AND (codex_session_id = '' OR codex_session_id = ?)`, candidate.CodexSessionID, candidate.CommitSHA, now, sessionID, candidate.CodexSessionID); err != nil {
		return CandidateHandoff{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = (SELECT version_id FROM ticket_sessions WHERE session_id = ?) AND issue_id = (SELECT issue_id FROM ticket_sessions WHERE session_id = ?) AND delivered = 0`, plan.StateWaitingReview, now, sessionID, sessionID); err != nil {
		return CandidateHandoff{}, err
	}
	push := DeliveryRequest{Operation: DeliveryPushCandidate, RunID: candidate.RunID, LeaseToken: candidate.LeaseToken, LeaseGeneration: generation, Repository: candidate.Publication.Repository, Branch: candidate.Publication.Branch, CommitSHA: candidate.CommitSHA, ExpectedRemoteHead: candidate.Publication.ExpectedRemoteHead, ExpectRemoteAbsent: candidate.Publication.ExpectRemoteAbsent}
	pr := DeliveryRequest{Operation: DeliveryUpsertPR, RunID: candidate.RunID, LeaseToken: candidate.LeaseToken, LeaseGeneration: generation, Repository: candidate.Publication.Repository, Branch: candidate.Publication.Branch, CommitSHA: candidate.CommitSHA, ExpectedRemoteHead: candidate.CommitSHA, Title: candidate.Publication.Title, Body: candidate.Publication.Body}
	pushKey, err := enqueueAcceptedDeliveryTx(ctx, tx, push, now)
	if err != nil {
		return CandidateHandoff{}, err
	}
	prKey, err := enqueueAcceptedDeliveryTx(ctx, tx, pr, now)
	if err != nil {
		return CandidateHandoff{}, err
	}
	if err := tx.Commit(); err != nil {
		return CandidateHandoff{}, err
	}
	return CandidateHandoff{PushOutboxKey: pushKey, PROutboxKey: prKey}, nil
}

func enqueueAcceptedDeliveryTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, now string) (string, error) {
	if request.RootNumber == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT p.root_issue_number
FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN plan_versions v ON v.version_id = s.version_id
JOIN plans p ON p.id = v.plan_id
WHERE r.run_id = ?`, request.RunID).Scan(&request.RootNumber); err != nil {
			return "", err
		}
	}
	key, err := deliveryKey(request)
	if err != nil {
		return "", err
	}
	request.IdempotencyKey = key
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_deliveries(version_id, issue_id, repository, branch, pull_request_number, pull_request_node_id, remote_head, created_at, updated_at)
SELECT s.version_id, s.issue_id, ?, ?, 0, '', '', ?, ? FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?
ON CONFLICT(version_id, issue_id) DO UPDATE SET repository = excluded.repository, branch = excluded.branch, updated_at = excluded.updated_at`, request.Repository, request.Branch, now, now, request.RunID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_outbox(idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at, next_attempt_at)
VALUES (?, ?, ?, ?, 0, '', ?, ?, ?)`, key, request.Operation, string(raw), OutboxPending, now, now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO accepted_candidate_outbox(outbox_key, run_id, created_at) VALUES (?, ?, ?)`, key, request.RunID, now); err != nil {
		return "", err
	}
	if err := insertDeliveryAuditTx(ctx, tx, request, "accepted", "queued with accepted candidate", time.Now().UTC()); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) RecordRunFailure(ctx context.Context, failure RunFailure) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if failure.RunID == "" || failure.LeaseToken == "" || failure.DiagnosticsPath == "" {
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
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation WHERE r.run_id = ? AND l.lease_token = ? AND r.state = ? AND l.state = ?`, failure.RunID, failure.LeaseToken, RunRunning, LeaseActive).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	now := formatTimestamp(failure.Now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_diagnostics(run_id, diagnostics_path, error, created_at) VALUES (?, ?, ?, ?)`, failure.RunID, failure.DiagnosticsPath, failure.Error, now); err != nil {
		return err
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
	result, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, failure.RunID)
	if err != nil {
		return err
	}
	claimedFeedback, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if claimedFeedback > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ?
WHERE (version_id, issue_id) = (SELECT s.version_id, s.issue_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?) AND state = ? AND delivered = 0`, plan.StateWaitingReview, now, failure.RunID, plan.StateRunning); err != nil {
			return err
		}
	}
	return tx.Commit()
}
