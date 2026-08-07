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
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

type DeliveryOperation string

const (
	DeliveryPushCandidate DeliveryOperation = "push_candidate"
	DeliveryUpsertPR      DeliveryOperation = "upsert_pull_request"
	DeliveryReplyEvidence DeliveryOperation = "reply_evidence"
	DeliveryProjectPlan   DeliveryOperation = "project_plan"
	DeliveryProjectInbox  DeliveryOperation = "project_workflow_inbox"
	DeliveryAddIssueLabel DeliveryOperation = "add_issue_label"

	OutboxPending    = "pending"
	OutboxProcessing = "processing"
	OutboxSucceeded  = "succeeded"
	OutboxRejected   = "rejected"
)

var (
	ErrDeliveryRejected     = errors.New("delivery command rejected")
	ErrNoActiveDeliveryPlan = fmt.Errorf("%w: workflow inbox repository has no active delivery plan", ErrDeliveryRejected)
	ErrDeliverySuperseded   = errors.New("delivery command was superseded")
	ErrInboxDeliveryPending = fmt.Errorf("%w: another workflow inbox projection is processing", ErrDeliveryInProgress)
	ErrDeliveryUncertain    = errors.New("delivery outcome is uncertain")
)

// DeliveryRequest is the schema-only command accepted from a Ticket Agent.
// The gateway derives all authoritative ownership fields from RunID and the
// current Run Lease before this request can reach an external writer.
type DeliveryRequest struct {
	Operation                 DeliveryOperation       `json:"operation"`
	RunID                     string                  `json:"run_id"`
	LeaseToken                string                  `json:"lease_token"`
	LeaseGeneration           int64                   `json:"lease_generation"`
	Repository                string                  `json:"repository"`
	RootNumber                int64                   `json:"root_number,omitempty"`
	Branch                    string                  `json:"branch,omitempty"`
	PullRequestNumber         int64                   `json:"pull_request_number,omitempty"`
	CommitSHA                 string                  `json:"commit_sha,omitempty"`
	ExpectedRemoteHead        string                  `json:"expected_remote_head,omitempty"`
	ExpectRemoteAbsent        bool                    `json:"expect_remote_absent,omitempty"`
	Title                     string                  `json:"title,omitempty"`
	Body                      string                  `json:"body,omitempty"`
	Evidence                  string                  `json:"evidence,omitempty"`
	PlanProjection            *plan.Projection        `json:"plan_projection,omitempty"`
	WorkflowQuestions         []plan.WorkflowQuestion `json:"workflow_questions,omitempty"`
	InboxPlanVersionID        string                  `json:"inbox_plan_version_id,omitempty"`
	InboxPlanVersionIDs       []string                `json:"inbox_plan_version_ids,omitempty"`
	InboxProjectionVersion    string                  `json:"inbox_projection_version,omitempty"`
	InboxProjectionGeneration int64                   `json:"inbox_projection_generation,omitempty"`
	InboxProjectionSuperseded bool                    `json:"-"`
	Label                     string                  `json:"label,omitempty"`
	IdempotencyKey            string                  `json:"idempotency_key,omitempty"`
}

type DeliveryTarget struct {
	VersionID               string
	TicketID                int64
	SessionID               string
	RunID                   string
	LeaseGeneration         int64
	Repository              string
	RootNumber              int64
	Branch                  string
	AcceptedCommit          string
	PullRequestNumber       int64
	PullRequestNodeID       string
	RemoteHead              string
	LeaseExpiresAt          time.Time
	InboxRecoveryAuthorized bool
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
	ClaimToken     string
	NextAttemptAt  *time.Time
	ReconcileOnly  bool
	Uncertain      bool
}

type DeliveryResult struct {
	RemoteHead        string
	PullRequestNumber int64
	PullRequestNodeID string
}

type TicketDelivery struct {
	VersionID         string
	IssueID           int64
	Repository        string
	PullRequestNumber int64
	CandidateCommit   string
	Branch            string
	RemoteHead        string
	ChecksETag        string
}

func (s *Store) TicketDelivery(ctx context.Context, versionID string, issueID int64) (TicketDelivery, error) {
	delivery, err := s.CandidateDelivery(ctx, versionID, issueID)
	if errors.Is(err, ErrNotFound) || (err == nil && delivery.PullRequestNumber == 0) {
		return TicketDelivery{}, ErrNotFound
	}
	return delivery, err
}

func (s *Store) CandidateDelivery(ctx context.Context, versionID string, issueID int64) (TicketDelivery, error) {
	var delivery TicketDelivery
	err := s.db.QueryRowContext(ctx, `SELECT d.version_id, d.issue_id, d.repository, d.pull_request_number, s.accepted_commit, d.branch, d.remote_head, d.checks_etag
FROM ticket_deliveries d
JOIN ticket_sessions s ON s.version_id = d.version_id AND s.issue_id = d.issue_id
WHERE d.version_id = ? AND d.issue_id = ? AND s.accepted_commit != ''`, versionID, issueID).
		Scan(&delivery.VersionID, &delivery.IssueID, &delivery.Repository, &delivery.PullRequestNumber, &delivery.CandidateCommit, &delivery.Branch, &delivery.RemoteHead, &delivery.ChecksETag)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketDelivery{}, ErrNotFound
	}
	return delivery, err
}

const maxDeliveryAttempts = 3

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
	outbox, err := s.enqueueDeliveryTx(ctx, tx, request, now)
	if err != nil {
		if commitErr := tx.Commit(); commitErr != nil {
			return DeliveryOutbox{}, commitErr
		}
		return DeliveryOutbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	return outbox, nil
}

func (s *Store) enqueueDeliveryTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, now time.Time) (DeliveryOutbox, error) {
	target, normalized, err := loadDeliveryTargetTx(ctx, tx, request, now, "", false)
	if err != nil {
		if auditErr := insertDeliveryAuditTx(ctx, tx, request, "rejected", err.Error(), now); auditErr != nil {
			return DeliveryOutbox{}, auditErr
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
	nowText := formatTimestamp(now)
	var existingJSON string
	var existing DeliveryOutbox
	var existingCreated, existingUpdated, existingCompleted, existingNext string
	var existingUncertain int
	err = tx.QueryRowContext(ctx, `SELECT id, idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at, COALESCE(completed_at, ''), claim_token, next_attempt_at, uncertain FROM delivery_outbox WHERE idempotency_key = ?`, key).
		Scan(&existing.ID, &existing.IdempotencyKey, &existing.Request.Operation, &existingJSON, &existing.State, &existing.Attempts, &existing.LastError, &existingCreated, &existingUpdated, &existingCompleted, &existing.ClaimToken, &existingNext, &existingUncertain)
	if err == nil {
		var decoded DeliveryRequest
		if err := json.Unmarshal([]byte(existingJSON), &decoded); err != nil {
			return DeliveryOutbox{}, err
		}
		existing.Request = decoded
		existing.Uncertain = existingUncertain != 0
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
		if existingNext != "" {
			value, parseErr := time.Parse(time.RFC3339Nano, existingNext)
			if parseErr != nil {
				return DeliveryOutbox{}, parseErr
			}
			existing.NextAttemptAt = &value
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveryOutbox{}, err
	}
	if target.TicketID != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_deliveries(version_id, issue_id, repository, branch, pull_request_number, pull_request_node_id, remote_head, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, '', '', ?, ?)
ON CONFLICT(version_id, issue_id) DO UPDATE SET repository = excluded.repository, branch = excluded.branch, updated_at = excluded.updated_at`,
			target.VersionID, target.TicketID, target.Repository, target.Branch, target.PullRequestNumber, nowText, nowText); err != nil {
			return DeliveryOutbox{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO delivery_outbox(idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at, next_attempt_at)
VALUES (?, ?, ?, ?, 0, '', ?, ?, ?)`, key, normalized.Operation, string(encoded), OutboxPending, nowText, nowText, nowText)
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
	target, _, err := loadDeliveryTargetTx(ctx, tx, request, now.UTC(), "", false)
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

func (s *Store) ExecuteDelivery(ctx context.Context, request DeliveryRequest, claimToken string, now func() time.Time, apply func(context.Context, DeliveryRequest) (DeliveryResult, error)) (DeliveryResult, error) {
	return s.executeDelivery(ctx, request, claimToken, now, apply, false)
}

func (s *Store) ReconcileDelivery(ctx context.Context, request DeliveryRequest, claimToken string, now func() time.Time, reconcile func(context.Context, DeliveryRequest) (DeliveryResult, error)) (DeliveryResult, error) {
	return s.executeDelivery(ctx, request, claimToken, now, reconcile, true)
}

func (s *Store) executeDelivery(ctx context.Context, request DeliveryRequest, claimToken string, now func() time.Time, apply func(context.Context, DeliveryRequest) (DeliveryResult, error), reconcileInbox bool) (DeliveryResult, error) {
	if apply == nil {
		return DeliveryResult{}, ErrInvalidClaim
	}
	if now == nil {
		now = time.Now
	}
	validatedAt := now().UTC()
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryResult{}, err
	}
	defer tx.Rollback()
	target, normalized, err := loadDeliveryTargetTx(ctx, tx, request, validatedAt, claimToken, reconcileInbox)
	if err != nil {
		return DeliveryResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryResult{}, err
	}
	operationCtx := ctx
	cancel := func() {}
	if !target.LeaseExpiresAt.IsZero() {
		remaining := target.LeaseExpiresAt.Sub(now().UTC())
		if remaining <= 0 {
			return DeliveryResult{}, fmt.Errorf("%w: delivery lease expired before external write", ErrDeliveryRejected)
		}
		operationCtx, cancel = context.WithTimeout(ctx, remaining)
	}
	defer cancel()
	deliveryResult, err := apply(operationCtx, normalized)
	if err != nil {
		if reconcileInbox && target.InboxRecoveryAuthorized && errors.Is(err, ErrDeliverySuperseded) {
			return DeliveryResult{}, nil
		}
		return DeliveryResult{}, err
	}
	if err := operationCtx.Err(); err != nil {
		return DeliveryResult{}, fmt.Errorf("%w: delivery lease expired during external write: %v", ErrDeliveryUncertain, err)
	}
	return deliveryResult, nil
}

func (s *Store) DeliveryOutbox(ctx context.Context, key string) (DeliveryOutbox, error) {
	var result DeliveryOutbox
	var raw, created, updated, completed, next string
	var uncertain int
	err := s.db.QueryRowContext(ctx, `SELECT id, idempotency_key, operation, request_json, state, attempts, last_error, created_at, updated_at, COALESCE(completed_at, ''), claim_token, next_attempt_at, uncertain FROM delivery_outbox WHERE idempotency_key = ?`, key).
		Scan(&result.ID, &result.IdempotencyKey, &result.Request.Operation, &raw, &result.State, &result.Attempts, &result.LastError, &created, &updated, &completed, &result.ClaimToken, &next, &uncertain)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryOutbox{}, ErrNotFound
	}
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if err := json.Unmarshal([]byte(raw), &result.Request); err != nil {
		return DeliveryOutbox{}, err
	}
	result.Uncertain = uncertain != 0
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
	if next != "" {
		value, parseErr := time.Parse(time.RFC3339Nano, next)
		if parseErr != nil {
			return DeliveryOutbox{}, parseErr
		}
		result.NextAttemptAt = &value
	}
	return result, nil
}

func (s *Store) RecoverableUncertainInboxDeliveryKeys(ctx context.Context, repository string) ([]string, error) {
	if repository == "" {
		return nil, ErrInvalidClaim
	}
	rows, err := s.db.QueryContext(ctx, `SELECT idempotency_key FROM delivery_outbox
WHERE operation = ? AND state = ? AND uncertain != 0
  AND json_valid(request_json)
  AND json_extract(request_json, '$.repository') = ?
ORDER BY updated_at, idempotency_key`, DeliveryProjectInbox, OutboxRejected, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) DueDeliveryOutboxKeys(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 32
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.idempotency_key FROM delivery_outbox o
WHERE o.state IN (?, ?) AND (o.state = ? OR o.next_attempt_at <= ?)
AND (o.operation != ? OR EXISTS (
  SELECT 1 FROM delivery_outbox pushed
  WHERE pushed.operation = ? AND pushed.state = ?
  AND json_extract(pushed.request_json, '$.run_id') = json_extract(o.request_json, '$.run_id')
))
ORDER BY o.created_at, CASE o.operation WHEN ? THEN 0 ELSE 1 END, o.idempotency_key LIMIT ?`,
		OutboxPending, OutboxProcessing, OutboxProcessing, formatTimestamp(now), DeliveryUpsertPR, DeliveryPushCandidate, OutboxSucceeded, DeliveryPushCandidate, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) PendingTicketDeliveries(ctx context.Context, repository string) ([]TicketDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.version_id, d.issue_id, d.repository, d.pull_request_number, s.accepted_commit, d.branch, d.remote_head, d.checks_etag
FROM ticket_deliveries d
JOIN ticket_runtime r ON r.version_id = d.version_id AND r.issue_id = d.issue_id
JOIN ticket_sessions s ON s.version_id = d.version_id AND s.issue_id = d.issue_id
JOIN plan_versions v ON v.version_id = d.version_id
JOIN plans p ON p.id = v.plan_id
WHERE d.repository = ? AND d.pull_request_number > 0 AND r.delivered = 0 AND r.state != ? AND s.accepted_commit != ''
AND `+currentActiveUnfrozenPlanPredicate, repository, plan.StateCancelled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []TicketDelivery
	for rows.Next() {
		var delivery TicketDelivery
		if err := rows.Scan(&delivery.VersionID, &delivery.IssueID, &delivery.Repository, &delivery.PullRequestNumber, &delivery.CandidateCommit, &delivery.Branch, &delivery.RemoteHead, &delivery.ChecksETag); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (s *Store) RecordPullRequestChecksETag(ctx context.Context, versionID string, issueID int64, etag string) error {
	if versionID == "" || issueID == 0 {
		return ErrInvalidClaim
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ticket_deliveries SET checks_etag = ?, updated_at = ? WHERE version_id = ? AND issue_id = ?`, etag, formatTimestamp(time.Now()), versionID, issueID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ClaimDeliveryOutbox(ctx context.Context, key string, now time.Time) (DeliveryOutbox, error) {
	if err := s.EnsureGatewayDispatcher(ctx, "legacy-gateway-dispatcher", now); err != nil {
		return DeliveryOutbox{}, err
	}
	return s.claimDeliveryOutbox(ctx, key, "legacy-gateway-dispatcher", now)
}

func (s *Store) ClaimDeliveryOutboxForDispatcher(ctx context.Context, key, dispatcherToken string, now time.Time) (DeliveryOutbox, error) {
	if dispatcherToken == "" {
		return DeliveryOutbox{}, errors.New("Gateway dispatcher token is required")
	}
	return s.claimDeliveryOutbox(ctx, key, dispatcherToken, now)
}

func (s *Store) claimDeliveryOutbox(ctx context.Context, key, dispatcherToken string, now time.Time) (DeliveryOutbox, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
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
	var writesPaused int
	var pauseReason string
	if err := tx.QueryRowContext(ctx, `SELECT writes_paused, reason FROM gateway_runtime WHERE singleton = 1`).Scan(&writesPaused, &pauseReason); err != nil {
		return DeliveryOutbox{}, err
	}
	if writesPaused != 0 {
		return DeliveryOutbox{}, fmt.Errorf("%w: %s", ErrGatewayWritesPaused, pauseReason)
	}
	var state, updatedText, nextAttemptText, raw, previousClaimToken, previousDispatcherToken string
	var attempts int
	var uncertain int
	if err := tx.QueryRowContext(ctx, `SELECT state, updated_at, attempts, next_attempt_at, request_json, claim_token, dispatcher_token, uncertain FROM delivery_outbox WHERE idempotency_key = ?`, key).Scan(&state, &updatedText, &attempts, &nextAttemptText, &raw, &previousClaimToken, &previousDispatcherToken, &uncertain); errors.Is(err, sql.ErrNoRows) {
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
		recoverable, recoverErr := deliveryOutboxClaimRecoverableTx(ctx, tx, raw, updatedText, previousDispatcherToken, now)
		if recoverErr != nil {
			return DeliveryOutbox{}, recoverErr
		}
		if !recoverable {
			return DeliveryOutbox{}, ErrDeliveryInProgress
		}
	}
	if state == OutboxPending && nextAttemptText != "" {
		nextAttempt, parseErr := time.Parse(time.RFC3339Nano, nextAttemptText)
		if parseErr != nil {
			return DeliveryOutbox{}, parseErr
		}
		if nextAttempt.After(now) {
			return DeliveryOutbox{}, ErrDeliveryInProgress
		}
	}
	reconcileOnly := uncertain != 0 || state == OutboxProcessing
	var request DeliveryRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return DeliveryOutbox{}, err
	}
	if attempts >= maxDeliveryAttempts {
		lastError := inboxDeliveryRecoveryReason(request, key, "delivery retries exhausted", uncertain != 0)
		if err := s.markDeliveryNeedsAttentionTx(ctx, tx, request, key, uncertain != 0, lastError, now); err != nil {
			return DeliveryOutbox{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, last_error = ?, claim_token = '', completed_at = ?, updated_at = ? WHERE idempotency_key = ?`, OutboxRejected, lastError, formatTimestamp(now), formatTimestamp(now), key); err != nil {
			return DeliveryOutbox{}, err
		}
		if err := tx.Commit(); err != nil {
			return DeliveryOutbox{}, err
		}
		return DeliveryOutbox{}, fmt.Errorf("%w: %s", ErrDeliveryRejected, lastError)
	}
	claimToken, err := randomID("outbox-claim")
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if request.Operation == DeliveryProjectInbox {
		var processing int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM delivery_outbox
WHERE idempotency_key != ? AND operation = ?
AND json_extract(request_json, '$.repository') = ?
AND (state = ? OR (
    state IN (?, ?) AND uncertain != 0
    AND COALESCE(CAST(json_extract(request_json, '$.inbox_projection_generation') AS INTEGER), 0) < ?
)) LIMIT 1`, key, DeliveryProjectInbox, request.Repository, OutboxProcessing, OutboxPending, OutboxRejected, request.InboxProjectionGeneration).Scan(&processing)
		if err == nil {
			return DeliveryOutbox{}, ErrInboxDeliveryPending
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return DeliveryOutbox{}, err
		}
	}
	claimedDispatcherToken := ""
	if request.RunID == "" {
		var activeDispatcherToken, expiresText string
		if err := tx.QueryRowContext(ctx, `SELECT dispatcher_token, dispatcher_expires_at FROM gateway_runtime WHERE singleton = 1`).Scan(&activeDispatcherToken, &expiresText); err != nil {
			return DeliveryOutbox{}, err
		}
		if activeDispatcherToken != dispatcherToken || expiresText == "" {
			return DeliveryOutbox{}, ErrDeliveryInProgress
		}
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
		if parseErr != nil {
			return DeliveryOutbox{}, parseErr
		}
		if !expiresAt.After(now) {
			return DeliveryOutbox{}, ErrDeliveryInProgress
		}
		claimedDispatcherToken = dispatcherToken
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, attempts = attempts + 1, claim_token = ?, dispatcher_token = ?, uncertain = CASE WHEN state = ? THEN 1 ELSE uncertain END, updated_at = ? WHERE idempotency_key = ? AND ((state = ? AND claim_token = ? AND dispatcher_token = ?) OR (state = ? AND claim_token = ? AND dispatcher_token = ?))`, OutboxProcessing, claimToken, claimedDispatcherToken, OutboxProcessing, formatTimestamp(now), key, OutboxPending, previousClaimToken, previousDispatcherToken, OutboxProcessing, previousClaimToken, previousDispatcherToken)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return DeliveryOutbox{}, ErrDeliveryInProgress
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOutbox{}, err
	}
	claimed, err := s.DeliveryOutbox(ctx, key)
	claimed.ReconcileOnly = reconcileOnly
	return claimed, err
}

func deliveryOutboxClaimRecoverableTx(ctx context.Context, tx *sql.Tx, raw, updatedText, dispatcherToken string, now time.Time) (bool, error) {
	var request DeliveryRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return false, err
	}
	if request.RunID == "" {
		if dispatcherToken == "" {
			return false, nil
		}
		var activeDispatcherToken, expiresText string
		err := tx.QueryRowContext(ctx, `SELECT dispatcher_token, dispatcher_expires_at FROM gateway_runtime WHERE singleton = 1`).Scan(&activeDispatcherToken, &expiresText)
		if err != nil {
			return false, err
		}
		if activeDispatcherToken != dispatcherToken || expiresText == "" {
			return true, nil
		}
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
		if parseErr != nil {
			return false, parseErr
		}
		return !expiresAt.After(now), nil
	}
	var expiresText string
	err := tx.QueryRowContext(ctx, `SELECT expires_at FROM run_leases WHERE lease_token = ? AND run_id = ? AND generation = ?`, request.LeaseToken, request.RunID, request.LeaseGeneration).Scan(&expiresText)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return false, err
	}
	return !expiresAt.After(now), nil
}

func (s *Store) FinishDeliveryOutbox(ctx context.Context, key, claimToken, state, lastError string, now time.Time) error {
	return s.finishDeliveryOutbox(ctx, key, claimToken, state, lastError, false, now, true, time.Time{})
}

func (s *Store) RejectDeliveryOutbox(ctx context.Context, key, claimToken, lastError string, uncertain bool, now time.Time) error {
	return s.finishDeliveryOutbox(ctx, key, claimToken, OutboxRejected, lastError, uncertain, now, true, time.Time{})
}

func (s *Store) MarkDeliveryOutboxUncertain(ctx context.Context, key, claimToken, lastError string, now time.Time) error {
	return s.finishDeliveryOutbox(ctx, key, claimToken, OutboxPending, lastError, true, now, true, time.Time{})
}

func (s *Store) RequeueDeliveryOutboxClaim(ctx context.Context, key, claimToken, lastError string, uncertain bool, now time.Time) error {
	return s.finishDeliveryOutbox(ctx, key, claimToken, OutboxPending, lastError, uncertain, now, false, time.Time{})
}

func (s *Store) DeferDeliveryOutbox(ctx context.Context, key, claimToken, lastError string, uncertain bool, retryAt, now time.Time) error {
	return s.finishDeliveryOutbox(ctx, key, claimToken, OutboxPending, lastError, uncertain, now, true, retryAt)
}

func (s *Store) RecoverUncertainInboxDelivery(ctx context.Context, repository, key, questionID, answer string, now time.Time) (DeliveryOutbox, error) {
	if repository == "" || key == "" || questionID == "" || answer == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	leaseToken, leaseNow, err := s.acquireGitHubPollMutationLease(ctx, repository)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	outbox, recoveryErr := s.recoverUncertainInboxDeliveryLeased(ctx, repository, key, questionID, answer, now, leaseToken, leaseNow)
	return outbox, errors.Join(recoveryErr, s.releaseGitHubPollMutationLease(ctx, repository, leaseToken))
}

func (s *Store) RecoverUncertainInboxDeliveryQuestionLeased(ctx context.Context, repository, questionID, answer string, now time.Time, leaseToken string, leaseNow time.Time) (DeliveryOutbox, error) {
	if repository == "" || questionID == "" || answer == "" {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return s.recoverUncertainInboxDeliveryLeased(ctx, repository, "", questionID, answer, now, leaseToken, leaseNow)
}

func (s *Store) UncertainInboxDeliveryRecoveryQuestionID(ctx context.Context, repository, key string) (string, error) {
	if repository == "" || key == "" {
		return "", ErrInvalidClaim
	}
	var questionID string
	err := s.db.QueryRowContext(ctx, `SELECT recovery.question_id
FROM inbox_delivery_recovery_questions recovery
JOIN workflow_questions question ON question.question_id = recovery.question_id
WHERE recovery.idempotency_key = ? AND question.repository = ? AND question.state = 'open'`, key, repository).Scan(&questionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return questionID, err
}

func (s *Store) recoverUncertainInboxDeliveryLeased(ctx context.Context, repository, key, questionID, answer string, now time.Time, leaseToken string, leaseNow time.Time) (DeliveryOutbox, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	defer tx.Rollback()
	byQuestion := key == ""
	if byQuestion {
		err = tx.QueryRowContext(ctx, `SELECT recovery.idempotency_key
FROM inbox_delivery_recovery_questions recovery
JOIN workflow_questions question ON question.question_id = recovery.question_id
WHERE recovery.question_id = ? AND question.repository = ? AND question.kind = 'inbox_delivery_recovery'`, questionID, repository).Scan(&key)
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryOutbox{}, ErrNotFound
		}
		if err != nil {
			return DeliveryOutbox{}, err
		}
	}
	var state, priorAnswer string
	err = tx.QueryRowContext(ctx, `SELECT question.state, question.answer
FROM inbox_delivery_recovery_questions recovery
JOIN workflow_questions question ON question.question_id = recovery.question_id
WHERE recovery.idempotency_key = ? AND recovery.question_id = ? AND question.repository = ? AND question.kind = 'inbox_delivery_recovery'`,
		key, questionID, repository).Scan(&state, &priorAnswer)
	if errors.Is(err, sql.ErrNoRows) {
		if byQuestion {
			return DeliveryOutbox{}, ErrNotFound
		}
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if state == "answered" && priorAnswer != answer {
		return DeliveryOutbox{}, ErrInvalidClaim
	}
	if err := s.answerWorkflowQuestionTx(ctx, tx, repository, questionID, answer, now, leaseToken, leaseNow); err != nil {
		return DeliveryOutbox{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_outbox
SET state = ?, attempts = 0, last_error = '', claim_token = '', dispatcher_token = '',
	    next_attempt_at = ?, updated_at = ?, completed_at = NULL
WHERE idempotency_key = ? AND operation = ? AND state = ? AND uncertain != 0
  AND json_extract(request_json, '$.repository') = ?`,
		OutboxPending, formatTimestamp(now), formatTimestamp(now), key, DeliveryProjectInbox, OutboxRejected, repository)
	if err != nil {
		return DeliveryOutbox{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var authorized int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM delivery_audits WHERE idempotency_key = ? AND decision = 'recovery_authorized' AND reason = ?
)`, key, questionID).Scan(&authorized); err != nil {
			return DeliveryOutbox{}, err
		}
		if authorized == 0 || state != "answered" {
			return DeliveryOutbox{}, ErrInvalidClaim
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_audits(idempotency_key, operation, run_id, lease_generation, decision, reason, created_at)
	SELECT idempotency_key, operation, '', 0, 'recovery_authorized', ?, ?
	FROM delivery_outbox WHERE idempotency_key = ? AND NOT EXISTS (
	    SELECT 1 FROM delivery_audits WHERE idempotency_key = ? AND decision = 'recovery_authorized' AND reason = ?
	)`, questionID, formatTimestamp(now), key, key, questionID); err != nil {
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

func (s *Store) finishDeliveryOutbox(ctx context.Context, key, claimToken, state, lastError string, uncertain bool, now time.Time, requireDispatcher bool, retryAt time.Time) error {
	if state != OutboxPending && state != OutboxSucceeded && state != OutboxRejected {
		return ErrInvalidClaim
	}
	if claimToken == "" {
		return ErrFencingConflict
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
	var attempts int
	var raw, dispatcherToken string
	if err := tx.QueryRowContext(ctx, `SELECT attempts, request_json, dispatcher_token FROM delivery_outbox WHERE idempotency_key = ? AND state = ? AND claim_token = ?`, key, OutboxProcessing, claimToken).Scan(&attempts, &raw, &dispatcherToken); errors.Is(err, sql.ErrNoRows) {
		return ErrFencingConflict
	} else if err != nil {
		return err
	}
	var request DeliveryRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return err
	}
	if requireDispatcher {
		if err := ensureControlPlaneDispatcherTx(ctx, tx, request, dispatcherToken, now); err != nil {
			return err
		}
	}
	completed := ""
	nextAttempt := formatTimestamp(now)
	if state == OutboxSucceeded || state == OutboxRejected {
		completed = formatTimestamp(now)
		if state == OutboxRejected {
			lastError = inboxDeliveryRecoveryReason(request, key, lastError, uncertain)
			if request.Operation == DeliveryProjectInbox && uncertain {
				if err := s.markDeliveryNeedsAttentionTx(ctx, tx, request, key, uncertain, lastError, now); err != nil {
					return err
				}
			} else if (request.Operation == DeliveryPushCandidate || request.Operation == DeliveryUpsertPR) && request.RunID != "" {
				if err := s.markDeliveryNeedsAttentionTx(ctx, tx, request, key, uncertain, lastError, now); err != nil {
					return err
				}
			}
		}
	} else if attempts >= maxDeliveryAttempts {
		state = OutboxRejected
		completed = formatTimestamp(now)
		lastError = "delivery retries exhausted: " + lastError
		lastError = inboxDeliveryRecoveryReason(request, key, lastError, uncertain)
		if err := s.markDeliveryNeedsAttentionTx(ctx, tx, request, key, uncertain, lastError, now); err != nil {
			return err
		}
	} else {
		nextAttempt = formatTimestamp(now.Add(time.Second * time.Duration(1<<(attempts-1))))
		if retryAt.After(now) {
			nextAttempt = formatTimestamp(retryAt.UTC())
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, last_error = ?, claim_token = '', next_attempt_at = ?, updated_at = ?, completed_at = CASE WHEN ? = '' THEN completed_at ELSE ? END, uncertain = ? WHERE idempotency_key = ? AND state = ? AND claim_token = ?`, state, lastError, nextAttempt, formatTimestamp(now), completed, completed, boolInt(uncertain), key, OutboxProcessing, claimToken)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrFencingConflict
	}
	return tx.Commit()
}

func (s *Store) CompleteDeliveryOutbox(ctx context.Context, key, claimToken string, deliveryResult DeliveryResult, now time.Time) error {
	if claimToken == "" {
		return ErrFencingConflict
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
	var raw, dispatcherToken string
	if err := tx.QueryRowContext(ctx, `SELECT request_json, dispatcher_token FROM delivery_outbox WHERE idempotency_key = ? AND state = ? AND claim_token = ?`, key, OutboxProcessing, claimToken).Scan(&raw, &dispatcherToken); errors.Is(err, sql.ErrNoRows) {
		return ErrFencingConflict
	} else if err != nil {
		return err
	}
	var request DeliveryRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return err
	}
	if err := ensureControlPlaneDispatcherTx(ctx, tx, request, dispatcherToken, now); err != nil {
		return err
	}
	if deliveryResult.PullRequestNumber != 0 && request.Operation == DeliveryUpsertPR {
		result, err := tx.ExecContext(ctx, `UPDATE ticket_deliveries SET pull_request_number = ?, pull_request_node_id = ?, remote_head = ?, updated_at = ?
WHERE (version_id, issue_id) = (SELECT s.version_id, s.issue_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?)
AND repository = ? AND branch = ?`, deliveryResult.PullRequestNumber, deliveryResult.PullRequestNodeID, deliveryResult.RemoteHead, formatTimestamp(now), request.RunID, request.Repository, request.Branch)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrNotFound
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, last_error = '', claim_token = '', uncertain = 0, updated_at = ?, completed_at = ? WHERE idempotency_key = ? AND state = ? AND claim_token = ?`, OutboxSucceeded, formatTimestamp(now), formatTimestamp(now), key, OutboxProcessing, claimToken)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrFencingConflict
	}
	return tx.Commit()
}

func ensureControlPlaneDispatcherTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, dispatcherToken string, now time.Time) error {
	if request.RunID != "" {
		return nil
	}
	if dispatcherToken == "" {
		return ErrFencingConflict
	}
	var activeToken, expiresText string
	if err := tx.QueryRowContext(ctx, `SELECT dispatcher_token, dispatcher_expires_at FROM gateway_runtime WHERE singleton = 1`).Scan(&activeToken, &expiresText); err != nil {
		return err
	}
	if activeToken != dispatcherToken || expiresText == "" {
		return ErrFencingConflict
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return err
	}
	if !expiresAt.After(now) {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) markDeliveryNeedsAttentionTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, key string, uncertain bool, reason string, now time.Time) error {
	if request.RunID == "" {
		if request.Operation == DeliveryProjectInbox {
			versionIDs, err := workflowInboxDeliveryPlanVersions(ctx, tx, request.Repository)
			if err != nil {
				return err
			}
			for _, versionID := range versionIDs {
				if err := markPlanNeedsAttentionTx(ctx, tx, versionID, reason, now); err != nil {
					return err
				}
			}
			if uncertain {
				if err := ensureUncertainInboxRecoveryQuestionTx(ctx, tx, request, key, reason, now); err != nil {
					return err
				}
			}
			if len(versionIDs) > 0 {
				if _, err := s.queueWorkflowInboxProjectionTx(ctx, tx, request.Repository, now); err != nil {
					return err
				}
			}
		}
		return insertDeliveryAuditTx(ctx, tx, request, "needs_attention", reason, now)
	}
	var delivered int
	err := tx.QueryRowContext(ctx, `SELECT rt.delivered FROM ticket_runtime rt
JOIN worker_runs r ON r.run_id = ?
JOIN ticket_sessions s ON s.session_id = r.session_id
WHERE rt.version_id = s.version_id AND rt.issue_id = s.issue_id`, request.RunID).Scan(&delivered)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if delivered != 0 {
		return nil
	}
	var versionID string
	var issueID int64
	if err := tx.QueryRowContext(ctx, `SELECT s.version_id, s.issue_id FROM worker_runs r JOIN ticket_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?`, request.RunID).Scan(&versionID, &issueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := markTicketNeedsAttentionTx(ctx, tx, versionID, issueID, reason, now); err != nil {
		return err
	}
	return insertDeliveryAuditTx(ctx, tx, request, "needs_attention", reason, now)
}

func ensureUncertainInboxRecoveryQuestionTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, key, reason string, now time.Time) error {
	if request.Repository == "" || key == "" {
		return ErrInvalidClaim
	}
	var existing, existingState string
	err := tx.QueryRowContext(ctx, `SELECT recovery.question_id, question.state
FROM inbox_delivery_recovery_questions recovery
JOIN workflow_questions question ON question.question_id = recovery.question_id
WHERE recovery.idempotency_key = ?`, key).Scan(&existing, &existingState)
	if err == nil && existingState == "open" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	candidates := append([]string(nil), request.InboxPlanVersionIDs...)
	if request.InboxPlanVersionID != "" {
		candidates = append(candidates, request.InboxPlanVersionID)
	}
	versionID := ""
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		var valid int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM plan_versions v JOIN plans p ON p.id = v.plan_id
    WHERE v.version_id = ? AND p.repository = ?
)`, candidate, request.Repository).Scan(&valid); err != nil {
			return err
		}
		if valid != 0 {
			versionID = candidate
			break
		}
	}
	if versionID == "" {
		err := tx.QueryRowContext(ctx, `SELECT current_version_id FROM plans WHERE repository = ? ORDER BY current_version_id LIMIT 1`, request.Repository).Scan(&versionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM workflow_questions
WHERE repository = ? AND issue_id = 0 AND kind = 'inbox_delivery_recovery'`, request.Repository).Scan(&generation); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(key))
	questionID := fmt.Sprintf("inbox-delivery-recovery-%s-g%d", hex.EncodeToString(digest[:]), generation)
	prompt := fmt.Sprintf("Workflow Inbox delivery %s has an uncertain remote outcome. Reply to this id-addressed question to authorize reconciliation after verifying the remote state. %s", key, reason)
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, generation, prompt, state, created_at)
VALUES (?, ?, ?, 0, 'inbox_delivery_recovery', ?, ?, 'open', ?)`, questionID, request.Repository, versionID, generation, prompt, formatTimestamp(now)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_delivery_recovery_questions(idempotency_key, question_id) VALUES (?, ?)
ON CONFLICT(idempotency_key) DO UPDATE SET question_id = excluded.question_id`, key, questionID)
	return err
}

func inboxDeliveryRecoveryReason(request DeliveryRequest, key, reason string, uncertain bool) string {
	if !uncertain || request.Operation != DeliveryProjectInbox || request.RunID != "" {
		return reason
	}
	return fmt.Sprintf("%s. List the stable recovery question with workflow recover-inbox-delivery --repository %s, answer it explicitly, then retry with --delivery %s --question <id> --answer retry.", reason, request.Repository, key)
}

func (s *Store) RecordDeliveryMapping(ctx context.Context, request DeliveryRequest, number int64, nodeID, remoteHead string, now time.Time) error {
	if number <= 0 {
		return ErrInvalidClaim
	}
	target, err := s.ValidateDelivery(ctx, request, now)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ticket_deliveries SET pull_request_number = ?, pull_request_node_id = ?, remote_head = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND repository = ? AND branch = ?`, number, nodeID, remoteHead, formatTimestamp(now), target.VersionID, target.TicketID, target.Repository, target.Branch)
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
	_, execErr := s.db.ExecContext(ctx, `INSERT INTO delivery_audits(idempotency_key, operation, run_id, lease_generation, decision, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, key, request.Operation, request.RunID, request.LeaseGeneration, decision, reason, formatTimestamp(now))
	return execErr
}

func loadDeliveryTargetTx(ctx context.Context, tx *sql.Tx, request DeliveryRequest, now time.Time, admittedClaimToken string, reconcileInbox bool) (DeliveryTarget, DeliveryRequest, error) {
	if request.Operation == DeliveryProjectInbox && request.RunID == "" {
		if request.Repository == "" || request.RootNumber != 0 || request.PlanProjection != nil || request.Label != "" || request.Body != "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: workflow inbox projection is incomplete", ErrDeliveryRejected)
		}
		admittedUncertain := 0
		if admittedClaimToken != "" {
			key, keyErr := deliveryKey(request)
			if keyErr != nil {
				return DeliveryTarget{}, request, keyErr
			}
			if request.IdempotencyKey != key {
				return DeliveryTarget{}, request, ErrFencingConflict
			}
			var admitted int
			err := tx.QueryRowContext(ctx, `SELECT 1, uncertain FROM delivery_outbox
WHERE idempotency_key = ? AND operation = ? AND state = ? AND claim_token = ? LIMIT 1`, request.IdempotencyKey, DeliveryProjectInbox, OutboxProcessing, admittedClaimToken).Scan(&admitted, &admittedUncertain)
			if errors.Is(err, sql.ErrNoRows) {
				return DeliveryTarget{}, request, ErrFencingConflict
			}
			if err != nil {
				return DeliveryTarget{}, request, err
			}
		}
		if reconcileInbox {
			if admittedClaimToken == "" || admittedUncertain == 0 {
				return DeliveryTarget{}, request, ErrFencingConflict
			}
		}
		activeVersionIDs, err := workflowInboxDeliveryPlanVersions(ctx, tx, request.Repository)
		if err != nil {
			return DeliveryTarget{}, request, err
		}
		questions, err := workflowInboxQuestions(ctx, tx, request.Repository)
		if err != nil {
			return DeliveryTarget{}, request, err
		}
		projectionVersion, err := workflowInboxProjectionVersion(questions)
		if err != nil {
			return DeliveryTarget{}, request, err
		}
		generation, generationErr := workflowInboxProjectionGenerationTx(ctx, tx, request.Repository, projectionVersion, activeVersionIDs, now, admittedClaimToken == "" && len(activeVersionIDs) > 0 && request.InboxProjectionGeneration == 0)
		if generationErr != nil {
			return DeliveryTarget{}, request, generationErr
		}
		if reconcileInbox && admittedClaimToken != "" && generation > 0 && request.InboxProjectionGeneration != generation {
			if len(activeVersionIDs) == 0 {
				return DeliveryTarget{}, request, ErrNoActiveDeliveryPlan
			}
			var recoveryAuthorized int
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1
FROM inbox_delivery_recovery_questions recovery
JOIN delivery_audits audit ON audit.idempotency_key = recovery.idempotency_key
    AND audit.decision = 'recovery_authorized'
    AND audit.reason = recovery.question_id
WHERE recovery.idempotency_key = ?
)`, request.IdempotencyKey).Scan(&recoveryAuthorized)
			if err != nil {
				return DeliveryTarget{}, request, err
			}
			request.InboxProjectionSuperseded = true
			return DeliveryTarget{Repository: request.Repository, InboxRecoveryAuthorized: recoveryAuthorized != 0}, request, nil
		}
		if generation == 0 || len(activeVersionIDs) == 0 && request.InboxProjectionGeneration == 0 {
			return DeliveryTarget{}, request, ErrNoActiveDeliveryPlan
		}
		if len(request.InboxPlanVersionIDs) > 0 {
			if !slices.Equal(request.InboxPlanVersionIDs, activeVersionIDs) {
				return DeliveryTarget{}, request, ErrNoActiveDeliveryPlan
			}
		} else if request.InboxPlanVersionID != "" && (len(activeVersionIDs) != 1 || request.InboxPlanVersionID != activeVersionIDs[0]) {
			return DeliveryTarget{}, request, ErrNoActiveDeliveryPlan
		}
		if request.InboxProjectionGeneration > 0 && request.InboxProjectionGeneration != generation {
			return DeliveryTarget{}, request, ErrDeliverySuperseded
		}
		if admittedClaimToken != "" && request.InboxProjectionGeneration != generation {
			return DeliveryTarget{}, request, ErrDeliverySuperseded
		}
		request.InboxPlanVersionIDs = activeVersionIDs
		if len(activeVersionIDs) == 1 {
			request.InboxPlanVersionID = activeVersionIDs[0]
		} else {
			request.InboxPlanVersionID = ""
		}
		request.WorkflowQuestions = WorkflowQuestionProjections(questions)
		request.InboxProjectionVersion = projectionVersion
		request.InboxProjectionGeneration = generation
		versionID := ""
		if len(activeVersionIDs) > 0 {
			versionID = activeVersionIDs[0]
		}
		return DeliveryTarget{VersionID: versionID, Repository: request.Repository}, request, nil
	}
	if (request.Operation == DeliveryProjectPlan || request.Operation == DeliveryAddIssueLabel) && request.RunID == "" {
		if request.Repository == "" || request.RootNumber <= 0 || request.PlanProjection == nil || request.PlanProjection.VersionID == "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: control-plane projection is incomplete", ErrDeliveryRejected)
		}
		var versionID string
		err := tx.QueryRowContext(ctx, `SELECT v.version_id FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id WHERE p.repository = ? AND p.root_issue_number = ?`, request.Repository, request.RootNumber).Scan(&versionID)
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryTarget{}, request, fmt.Errorf("%w: current plan version is missing", ErrDeliveryRejected)
		}
		if err != nil {
			return DeliveryTarget{}, request, err
		}
		if versionID != request.PlanProjection.VersionID {
			return DeliveryTarget{}, request, fmt.Errorf("%w: projection does not match the current plan version", ErrDeliveryRejected)
		}
		switch request.Operation {
		case DeliveryProjectPlan:
			if request.Body != "" || request.Label != "" {
				return DeliveryTarget{}, request, fmt.Errorf("%w: structured plan projection is required", ErrDeliveryRejected)
			}
		case DeliveryAddIssueLabel:
			if request.Label != plan.ActiveLabel || request.Body != "" {
				return DeliveryTarget{}, request, fmt.Errorf("%w: plan label is required", ErrDeliveryRejected)
			}
		}
		return DeliveryTarget{VersionID: versionID, Repository: request.Repository, RootNumber: request.RootNumber}, request, nil
	}
	if request.RunID == "" || request.LeaseToken == "" || request.LeaseGeneration <= 0 {
		return DeliveryTarget{}, request, fmt.Errorf("%w: run lease is required", ErrDeliveryRejected)
	}
	switch request.Operation {
	case DeliveryPushCandidate, DeliveryUpsertPR, DeliveryReplyEvidence, DeliveryProjectPlan, DeliveryAddIssueLabel:
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
JOIN ticket_runtime rt ON rt.version_id = s.version_id AND rt.issue_id = s.issue_id
LEFT JOIN ticket_deliveries td ON td.version_id = s.version_id AND td.issue_id = s.issue_id
WHERE r.run_id = ? AND s.current_run_id = r.run_id AND r.run_kind = ? AND r.state = ? AND l.lease_token = ? AND l.generation = ? AND l.state = ?
AND `+currentActiveUnfrozenPlanPredicate,
		request.RunID, RunDelivery, RunRunning, request.LeaseToken, request.LeaseGeneration, LeaseActive).
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
	target.LeaseExpiresAt = expiresAt
	if request.Repository != "" && request.Repository != target.Repository {
		return DeliveryTarget{}, request, fmt.Errorf("%w: repository does not belong to ticket", ErrDeliveryRejected)
	}
	request.Repository = target.Repository
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
		if (request.ExpectedRemoteHead == "") == !request.ExpectRemoteAbsent {
			return DeliveryTarget{}, request, fmt.Errorf("%w: exactly one remote head expectation is required", ErrDeliveryRejected)
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
		if request.ExpectedRemoteHead == "" || request.ExpectRemoteAbsent {
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
		if request.PlanProjection == nil || request.PlanProjection.VersionID != target.VersionID || request.Body != "" {
			return DeliveryTarget{}, request, fmt.Errorf("%w: matching structured plan projection is required", ErrDeliveryRejected)
		}
	case DeliveryAddIssueLabel:
		return DeliveryTarget{}, request, fmt.Errorf("%w: labels require the control-plane gateway", ErrDeliveryRejected)
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
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_audits(idempotency_key, operation, run_id, lease_generation, decision, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, key, request.Operation, request.RunID, request.LeaseGeneration, decision, reason, formatTimestamp(now))
	return err
}
