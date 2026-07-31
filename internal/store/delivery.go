package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type DeliveryOperation string

const (
	DeliveryPushCandidate DeliveryOperation = "push_candidate"
	DeliveryUpsertPR      DeliveryOperation = "upsert_pull_request"
	DeliveryReplyEvidence DeliveryOperation = "reply_evidence"
	DeliveryProjectPlan   DeliveryOperation = "project_plan"

	OutboxPending    = "pending"
	OutboxProcessing = "processing"
	OutboxSucceeded  = "succeeded"
	OutboxRejected   = "rejected"
)

var ErrDeliveryRejected = errors.New("delivery command rejected")

// DeliveryRequest is the schema-only command accepted from a Ticket Agent.
// The gateway derives all authoritative ownership fields from RunID and the
// current Run Lease before this request can reach an external writer.
type DeliveryRequest struct {
	Operation          DeliveryOperation `json:"operation"`
	RunID              string            `json:"run_id"`
	LeaseToken         string            `json:"lease_token"`
	LeaseGeneration    int64             `json:"lease_generation"`
	Repository         string            `json:"repository"`
	RootNumber         int64             `json:"root_number,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	PullRequestNumber  int64             `json:"pull_request_number,omitempty"`
	CommitSHA          string            `json:"commit_sha,omitempty"`
	ExpectedRemoteHead string            `json:"expected_remote_head,omitempty"`
	Title              string            `json:"title,omitempty"`
	Body               string            `json:"body,omitempty"`
	Evidence           string            `json:"evidence,omitempty"`
	IdempotencyKey     string            `json:"idempotency_key,omitempty"`
}

type DeliveryTarget struct {
	VersionID         string
	TicketID          int64
	SessionID         string
	RunID             string
	LeaseGeneration   int64
	Repository        string
	RootNumber        int64
	Branch            string
	AcceptedCommit    string
	PullRequestNumber int64
	PullRequestNodeID string
	RemoteHead        string
}

type DeliveryOutbox struct {
	ID             int64
	IdempotencyKey string
	Request        DeliveryRequest
	State          string
	Attempts       int
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

type DeliveryAudit struct {
	ID              int64
	IdempotencyKey  string
	Operation       DeliveryOperation
	RunID           string
	LeaseGeneration int64
	Decision        string
	Reason          string
	CreatedAt       time.Time
}

// EnqueueDelivery validates and persists a command and its stable outbox
// identity in one SQLite transaction. Repeating the exact command is safe.
func (s *Store) EnqueueDelivery(ctx context.Context, request DeliveryRequest, now time.Time) (DeliveryOutbox, error) {
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

	target, normalized, err := loadDeliveryTargetTx(ctx, tx, request, now)
	if err != nil {
		if auditErr := insertDeliveryAuditTx(ctx, tx, request, "rejected", err.Error(), now); auditErr != nil {
			return DeliveryOutbox{}, auditErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return DeliveryOutbox{}, commitErr
		}
		return DeliveryOutbox{}, err
	}
	_ = target
	keyRequest := normalized
	if (normalized.Operation == DeliveryUpsertPR || normalized.Operation == DeliveryReplyEvidence) && request.PullRequestNumber == 0 {
		// The mapping is derived state. It must not change the idempotency key
		// when a first create is retried after the PR number is persisted.
		keyRequest.PullRequestNumber = 0
	}
	key, err := deliveryKey(keyRequest)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	normalized.IdempotencyKey = key
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	nowText := now.Format(time.RFC3339Nano)
	var existingJSON string
	var existing DeliveryOutbox
	var existingCreated, existingUpdated, existingCompleted string
	err = tx.QueryRowContext(ctx, `SELECT id, idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at, COALESCE(completed_at, '') FROM delivery_outbox WHERE idempotency_key = ?`, key).
		Scan(&existing.ID, &existing.IdempotencyKey, &existing.Request.Operation, &existingJSON, &existing.State, &existing.Attempts, &existing.LastError, &existingCreated, &existingUpdated, &existingCompleted)
	if err == nil {
		var decoded DeliveryRequest
		if err := json.Unmarshal([]byte(existingJSON), &decoded); err != nil {
			return DeliveryOutbox{}, err
		}
		existing.Request = decoded
		existing.CreatedAt, err = time.Parse(time.RFC3339Nano, existingCreated)
		if err != nil {
			return DeliveryOutbox{}, err
		}
		existing.UpdatedAt, err = time.Parse(time.RFC3339Nano, existingUpdated)
		if err != nil {
			return DeliveryOutbox{}, err
		}
		if existingCompleted != "" {
			value, parseErr := time.Parse(time.RFC3339Nano, existingCompleted)
			if parseErr != nil {
				return DeliveryOutbox{}, parseErr
			}
			existing.CompletedAt = &value
		}
		if err := tx.Commit(); err != nil {
			return DeliveryOutbox{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveryOutbox{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_deliveries(version_id, issue_id, repository, branch, pull_request_number, pull_request_node_id, remote_head, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, '', '', ?, ?)
ON CONFLICT(version_id, issue_id) DO UPDATE SET repository = excluded.repository, branch = excluded.branch, updated_at = excluded.updated_at`,
		target.VersionID, target.TicketID, target.Repository, target.Branch, target.PullRequestNumber, nowText, nowText); err != nil {
		return DeliveryOutbox{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO delivery_outbox(idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, 0, '', ?, ?)`, key, normalized.Operation, string(encoded), OutboxPending, nowText, nowText)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := insertDeliveryAuditTx(ctx, tx, normalized, "accepted", "queued in durable outbox", now); err != nil {
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return DeliveryOutbox{ID: id, IdempotencyKey: key, Request: normalized, State: OutboxPending, CreatedAt: now, UpdatedAt: now}, nil
}

// ValidateDelivery rechecks the live lease and the accepted Candidate
// Revision immediately before an external write.
func (s *Store) ValidateDelivery(ctx context.Context, request DeliveryRequest, now time.Time) (DeliveryTarget, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryTarget{}, err
	}
	defer tx.Rollback()
	target, _, err := loadDeliveryTargetTx(ctx, tx, request, now.UTC())
	if err != nil {
		if auditErr := insertDeliveryAuditTx(ctx, tx, request, "rejected", err.Error(), now.UTC()); auditErr != nil {
			return DeliveryTarget{}, auditErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return DeliveryTarget{}, commitErr
		}
		return DeliveryTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryTarget{}, err
	}
	return target, nil
}

func (s *Store) DeliveryOutbox(ctx context.Context, key string) (DeliveryOutbox, error) {
	var result DeliveryOutbox
	var raw, created, updated, completed string
	err := s.db.QueryRowContext(ctx, `SELECT id, idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at, COALESCE(completed_at, '') FROM delivery_outbox WHERE idempotency_key = ?`, key).
		Scan(&result.ID, &result.IdempotencyKey, &result.Request.Operation, &raw, &result.State, &result.Attempts, &result.LastError, &created, &updated, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryOutbox{}, ErrNotFound
	}
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := json.Unmarshal([]byte(raw), &result.Request); err != nil {
		return DeliveryOutbox{}, err
	}
	result.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	result.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if completed != "" {
		value, parseErr := time.Parse(time.RFC3339Nano, completed)
		if parseErr != nil {
			return DeliveryOutbox{}, parseErr
		}
		result.CompletedAt = &value
	}
	return result, nil
}

func (s *Store) ClaimDeliveryOutbox(ctx context.Context, key string, now time.Time) (DeliveryOutbox, error) {
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
	var state, updatedText string
	if err := tx.QueryRowContext(ctx, `SELECT state, updated_at FROM delivery_outbox WHERE idempotency_key = ?`, key).Scan(&state, &updatedText); errors.Is(err, sql.ErrNoRows) {
		return DeliveryOutbox{}, ErrNotFound
	} else if err != nil {
		return DeliveryOutbox{}, err
	}
	if state == OutboxSucceeded || state == OutboxRejected {
		if err := tx.Commit(); err != nil {
			return DeliveryOutbox{}, err
		}
		return s.DeliveryOutbox(ctx, key)
	}
	if state == OutboxProcessing {
		updatedAt, parseErr := time.Parse(time.RFC3339Nano, updatedText)
		if parseErr != nil {
			return DeliveryOutbox{}, parseErr
		}
		if updatedAt.After(now.Add(-time.Minute)) {
			return DeliveryOutbox{}, ErrDeliveryInProgress
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, attempts = attempts + 1, updated_at = ? WHERE idempotency_key = ? AND (state = ? OR (state = ? AND updated_at <= ?))`, OutboxProcessing, now.Format(time.RFC3339Nano), key, OutboxPending, OutboxProcessing, now.Add(-time.Minute).Format(time.RFC3339Nano))
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return DeliveryOutbox{}, ErrDeliveryInProgress
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return s.DeliveryOutbox(ctx, key)
}

func (s *Store) FinishDeliveryOutbox(ctx context.Context, key, state, lastError string, now time.Time) error {
	if state != OutboxPending && state != OutboxSucceeded && state != OutboxRejected {
		return ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	completed := ""
	if state == OutboxSucceeded || state == OutboxRejected {
		completed = now.Format(time.RFC3339Nano)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, last_error = ?, updated_at = ?, completed_at = CASE WHEN ? = '' THEN completed_at ELSE ? END WHERE idempotency_key = ?`, state, lastError, now.Format(time.RFC3339Nano), completed, completed, key)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecordDeliveryMapping(ctx context.Context, request DeliveryRequest, number int64, nodeID, remoteHead string, now time.Time) error {
	if number <= 0 {
		return ErrInvalidClaim
	}
	target, err := s.ValidateDelivery(ctx, request, now)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ticket_deliveries SET pull_request_number = ?, pull_request_node_id = ?, remote_head = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND repository = ? AND branch = ?`, number, nodeID, remoteHead, now.UTC().Format(time.RFC3339Nano), target.VersionID, target.TicketID, target.Repository, target.Branch)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeliveryAudits(ctx context.Context, key string) ([]DeliveryAudit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, idempotency_key, operation, run_id, lease_generation, decision, reason, created_at FROM delivery_audits WHERE idempotency_key = ? ORDER BY id`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var audits []DeliveryAudit
	for rows.Next() {
		var audit DeliveryAudit
		var operation, created string
		if err := rows.Scan(&audit.ID, &audit.IdempotencyKey, &operation, &audit.RunID, &audit.LeaseGeneration, &audit.Decision, &audit.Reason, &created); err != nil {
			return nil, err
		}
		audit.Operation = DeliveryOperation(operation)
		audit.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		audits = append(audits, audit)
	}
	return audits, rows.Err()
}

func (s *Store) RecordDeliveryAudit(ctx context.Context, request DeliveryRequest, decision, reason string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	key := request.IdempotencyKey
	if key == "" {
		var keyErr error
		key, keyErr = deliveryKey(request)
		if keyErr != nil {
			return keyErr
		}
	}
	_, execErr := s.db.ExecContext(ctx, `INSERT INTO delivery_audits(idempotency_key, operation, run_id, lease_generation, decision, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, key, request.Operation, request.RunID, request.LeaseGeneration, decision, reason, now.Format(time.RFC3339Nano))
	return execErr
}

func loadDeliveryTargetTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, now time.Time) (DeliveryTarget, DeliveryRequest, error) {
	if request.RunID == "" || request.LeaseToken == "" || request.LeaseGeneration <= 0 || request.Repository == "" {
		return DeliveryTarget{}, request, fmt.Errorf("%w: run lease and repository are required", ErrDeliveryRejected)
	}
	switch request.Operation {
	case DeliveryPushCandidate, DeliveryUpsertPR, DeliveryReplyEvidence, DeliveryProjectPlan:
	default:
		return DeliveryTarget{}, request, fmt.Errorf("%w: unsupported operation %q", ErrDeliveryRejected, request.Operation)
	}
	var target DeliveryTarget
	var expiresText string
	var mappedNumber int64
	var mappedNode, mappedHead string
	err := tx.QueryRowContext(ctx, `SELECT p.current_version_id, s.issue_id, s.session_id, r.run_id, l.generation, p.repository, p.root_issue_number, s.branch, s.accepted_commit,
	COALESCE(td.pull_request_number, 0), COALESCE(td.pull_request_node_id, ''), COALESCE(td.remote_head, ''), l.expires_at
FROM worker_runs r
JOIN ticket_sessions s ON s.session_id = r.session_id
JOIN plan_versions v ON v.version_id = s.version_id
JOIN plans p ON p.id = v.plan_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
LEFT JOIN ticket_deliveries td ON td.version_id = s.version_id AND td.issue_id = s.issue_id
WHERE r.run_id = ? AND p.current_version_id = v.version_id AND s.current_run_id = r.run_id AND r.state = ? AND l.lease_token = ? AND l.generation = ? AND l.state = ?`,
		request.RunID, RunRunning, request.LeaseToken, request.LeaseGeneration, LeaseActive).
		Scan(&target.VersionID, &target.TicketID, &target.SessionID, &target.RunID, &target.LeaseGeneration, &target.Repository, &target.RootNumber, &target.Branch, &target.AcceptedCommit, &mappedNumber, &mappedNode, &mappedHead, &expiresText)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryTarget{}, request, fmt.Errorf("%w: lease is not current", ErrDeliveryRejected)
	}
	if err != nil {
		return DeliveryTarget{}, request, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil || !expiresAt.After(now) {
		return DeliveryTarget{}, request, fmt.Errorf("%w: lease is expired", ErrDeliveryRejected)
	}
	if request.Repository != target.Repository {
		return DeliveryTarget{}, request, fmt.Errorf("%w: repository does not belong to ticket", ErrDeliveryRejected)
	}
	if request.Branch != "" && request.Branch != target.Branch {
		return DeliveryTarget{}, request, fmt.Errorf("%w: branch does not belong to ticket", ErrDeliveryRejected)
	}
	request.Branch = target.Branch
	if request.Branch == "main" || request.Branch == "master" {
		return DeliveryTarget{}, request, fmt.Errorf("%w: direct integration-line writes are forbidden", ErrDeliveryRejected)
	}
	if request.RootNumber != 0 && request.RootNumber != target.RootNumber {
		return DeliveryTarget{}, request, fmt.Errorf("%w: plan root does not belong to ticket", ErrDeliveryRejected)
	}
	request.RootNumber = target.RootNumber
	target.PullRequestNumber, target.PullRequestNodeID, target.RemoteHead = mappedNumber, mappedNode, mappedHead
	switch request.Operation {
	case DeliveryPushCandidate:
		if request.CommitSHA == "" || request.CommitSHA != target.AcceptedCommit {
			return DeliveryTarget{}, request, fmt.Errorf("%w: commit is not the accepted Candidate Revision", ErrDeliveryRejected)
		}
		if request.ExpectedRemoteHead == "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: expected remote head is required", ErrDeliveryRejected)
		}
		if request.PullRequestNumber != 0 {
			return DeliveryTarget{}, request, fmt.Errorf("%w: candidate push cannot target a pull request", ErrDeliveryRejected)
		}
	case DeliveryUpsertPR:
		if request.CommitSHA == "" || request.CommitSHA != target.AcceptedCommit {
			return DeliveryTarget{}, request, fmt.Errorf("%w: commit is not the accepted Candidate Revision", ErrDeliveryRejected)
		}
		if request.Title == "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: pull request title is required", ErrDeliveryRejected)
		}
		if request.ExpectedRemoteHead == "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: expected remote head is required", ErrDeliveryRejected)
		}
		if mappedNumber != 0 && request.PullRequestNumber != 0 && request.PullRequestNumber != mappedNumber {
			return DeliveryTarget{}, request, fmt.Errorf("%w: pull request is not mapped to ticket", ErrDeliveryRejected)
		}
		if mappedNumber == 0 && request.PullRequestNumber != 0 {
			return DeliveryTarget{}, request, fmt.Errorf("%w: pull request is not mapped to ticket", ErrDeliveryRejected)
		}
		if request.PullRequestNumber == 0 {
			request.PullRequestNumber = mappedNumber
		}
	case DeliveryReplyEvidence:
		if request.Evidence == "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: evidence is required", ErrDeliveryRejected)
		}
		if mappedNumber == 0 {
			return DeliveryTarget{}, request, fmt.Errorf("%w: ticket has no mapped pull request", ErrDeliveryRejected)
		}
		if request.PullRequestNumber == 0 {
			request.PullRequestNumber = mappedNumber
		} else if request.PullRequestNumber != mappedNumber {
			return DeliveryTarget{}, request, fmt.Errorf("%w: pull request is not mapped to ticket", ErrDeliveryRejected)
		}
	case DeliveryProjectPlan:
		if request.Body == "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: plan projection body is required", ErrDeliveryRejected)
		}
	}
	if request.CommitSHA != "" && request.CommitSHA != target.AcceptedCommit {
		return DeliveryTarget{}, request, fmt.Errorf("%w: commit does not match accepted Candidate Revision", ErrDeliveryRejected)
	}
	return target, request, nil
}

func deliveryKey(request DeliveryRequest) (string, error) {
	request.LeaseToken = ""
	request.IdempotencyKey = ""
	b, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(b)
	return "delivery-" + hex.EncodeToString(hash[:]), nil
}

func insertDeliveryAuditTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, decision, reason string, now time.Time) error {
	key := request.IdempotencyKey
	if key == "" {
		key, _ = deliveryKey(request)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_audits(idempotency_key, operation, run_id, lease_generation, decision, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, key, request.Operation, request.RunID, request.LeaseGeneration, decision, reason, now.Format(time.RFC3339Nano))
	return err
}
