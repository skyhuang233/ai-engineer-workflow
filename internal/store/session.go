package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
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
		binding.AgentIdentity, binding.WorkspacePath, binding.CodexStatePath, binding.Branch, time.Now().UTC().Format(time.RFC3339Nano), binding.SessionID)
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
	result, err := s.db.ExecContext(ctx, `INSERT INTO worker_audits(run_id, container_id, image_digest, mounts_json, tool_versions_json, github_write_credentials, created_at)
SELECT r.run_id, ?, ?, ?, ?, ?, ? FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.run_id = ? AND l.lease_token = ? AND r.state = ? AND l.state = ?`, audit.ContainerID, audit.ImageDigest, string(mounts), string(versions), boolInt(audit.GitHubWriteCredentials), time.Now().UTC().Format(time.RFC3339Nano), audit.RunID, audit.LeaseToken, RunRunning, LeaseActive)
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
	err := s.db.QueryRowContext(ctx, `SELECT run_id, container_id, image_digest, mounts_json, tool_versions_json, github_write_credentials FROM worker_audits WHERE run_id = ?`, runID).
		Scan(&record.RunID, &record.ContainerID, &record.ImageDigest, &record.MountsJSON, &record.ToolVersionsJSON, &credentials)
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
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET codex_session_id = ?, updated_at = ? WHERE session_id = ?`, codexSessionID, time.Now().UTC().Format(time.RFC3339Nano), sessionID); err != nil {
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
	if candidate.RunID == "" || candidate.LeaseToken == "" || candidate.CodexSessionID == "" || candidate.CommitSHA == "" || len(candidate.StructuredOutput) == 0 {
		return ErrInvalidClaim
	}
	if !json.Valid(candidate.StructuredOutput) {
		return ErrInvalidClaim
	}
	if candidate.Now.IsZero() {
		candidate.Now = time.Now().UTC()
	} else {
		candidate.Now = candidate.Now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID, currentSessionID, existingCodexSessionID string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT r.session_id, s.session_id, s.codex_session_id, l.generation FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
	WHERE r.run_id = ? AND l.lease_token = ? AND r.state = ? AND l.state = ? AND l.expires_at > ?`, candidate.RunID, candidate.LeaseToken, RunRunning, LeaseActive, candidate.Now.Format(time.RFC3339Nano)).Scan(&sessionID, &currentSessionID, &existingCodexSessionID, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidClaim
	}
	if err != nil {
		return err
	}
	if sessionID != currentSessionID || generation <= 0 {
		return ErrInvalidClaim
	}
	if existingCodexSessionID != "" && existingCodexSessionID != candidate.CodexSessionID {
		return ErrSessionConflict
	}
	now := candidate.Now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO candidate_revisions(run_id, session_id, codex_session_id, commit_sha, structured_output, created_at) VALUES (?, ?, ?, ?, ?, ?)`, candidate.RunID, sessionID, candidate.CodexSessionID, candidate.CommitSHA, string(candidate.StructuredOutput), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded', finished_at = ? WHERE run_id = ? AND state = ?`, now, candidate.RunID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, candidate.RunID, candidate.LeaseToken, LeaseActive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_sessions SET codex_session_id = CASE WHEN codex_session_id = '' THEN ? ELSE codex_session_id END, accepted_commit = ?, updated_at = ? WHERE session_id = ? AND (codex_session_id = '' OR codex_session_id = ?)`, candidate.CodexSessionID, candidate.CommitSHA, now, sessionID, candidate.CodexSessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordRunFailure(ctx context.Context, failure RunFailure) error {
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
	now := failure.Now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_diagnostics(run_id, diagnostics_path, error, created_at) VALUES (?, ?, ?, ?)`, failure.RunID, failure.DiagnosticsPath, failure.Error, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'failed', finished_at = ? WHERE run_id = ? AND state = ?`, now, failure.RunID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, failure.RunID, failure.LeaseToken, LeaseActive); err != nil {
		return err
	}
	return tx.Commit()
}
