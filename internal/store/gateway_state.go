package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const GatewayCredentialInboxKey = "gateway-credential"
const gatewayCredentialQuestionKind = "gateway_credential"

const gatewayDispatcherLeaseTTL = 2 * time.Minute
const gatewayCredentialRotationLeaseTTL = 5 * time.Minute

type GatewayCredentialRotation struct {
	Owner      string
	Generation int64
	ExpiresAt  time.Time
}

type WorkflowInboxItem struct {
	Key       string
	Kind      string
	Title     string
	Body      string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Store) PauseGatewayWrites(ctx context.Context, reason string, now time.Time) error {
	return s.pauseGatewayWrites(ctx, "", "", reason, now, time.Time{})
}

func (s *Store) PauseGatewayWritesForGitHubPoll(ctx context.Context, repository, leaseToken, reason string, now, leaseNow time.Time) error {
	return s.pauseGatewayWrites(ctx, repository, leaseToken, reason, now, leaseNow)
}

func (s *Store) pauseGatewayWrites(ctx context.Context, repository, leaseToken, reason string, now, leaseNow time.Time) error {
	if reason == "" {
		return errors.New("Gateway pause reason is required")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if leaseToken != "" {
		if err := requireGitHubPollLeaseTx(ctx, tx, repository, leaseToken, leaseNow); err != nil {
			return err
		}
	}
	timestamp := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_runtime SET writes_paused = 1, reason = ?, updated_at = ? WHERE singleton = 1`, reason, timestamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_inbox(item_key, kind, title, body, state, created_at, updated_at)
VALUES (?, 'credential', 'Gateway Credential requires attention', ?, 'open', ?, ?)
ON CONFLICT(item_key) DO UPDATE SET body=excluded.body, state='open', updated_at=excluded.updated_at`,
		GatewayCredentialInboxKey, reason, timestamp, timestamp); err != nil {
		return err
	}
	if err := ensureGatewayCredentialQuestionsTx(ctx, tx, reason, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginGatewayCredentialRotation(ctx context.Context, owner, reason string, now time.Time) (GatewayCredentialRotation, error) {
	if owner == "" || reason == "" {
		return GatewayCredentialRotation{}, errors.New("Gateway Credential rotation owner and reason are required")
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
		return GatewayCredentialRotation{}, err
	}
	defer tx.Rollback()
	var activeOwner, expiresText string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT rotation_owner, rotation_generation, rotation_expires_at FROM gateway_runtime WHERE singleton = 1`).Scan(&activeOwner, &generation, &expiresText); err != nil {
		return GatewayCredentialRotation{}, err
	}
	if activeOwner != "" && expiresText != "" {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
		if parseErr != nil {
			return GatewayCredentialRotation{}, parseErr
		}
		if expiresAt.After(now) {
			return GatewayCredentialRotation{}, ErrDeliveryInProgress
		}
	}
	generation++
	expiresAt := now.Add(gatewayCredentialRotationLeaseTTL)
	timestamp := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_runtime SET writes_paused = 1, reason = ?, rotation_owner = ?, rotation_generation = ?, rotation_expires_at = ?, updated_at = ? WHERE singleton = 1`, reason, owner, generation, formatTimestamp(expiresAt), timestamp); err != nil {
		return GatewayCredentialRotation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_inbox(item_key, kind, title, body, state, created_at, updated_at)
VALUES (?, 'credential', 'Gateway Credential requires attention', ?, 'open', ?, ?)
ON CONFLICT(item_key) DO UPDATE SET body=excluded.body, state='open', updated_at=excluded.updated_at`, GatewayCredentialInboxKey, reason, timestamp, timestamp); err != nil {
		return GatewayCredentialRotation{}, err
	}
	if err := ensureGatewayCredentialQuestionsTx(ctx, tx, reason, now); err != nil {
		return GatewayCredentialRotation{}, err
	}
	if err := tx.Commit(); err != nil {
		return GatewayCredentialRotation{}, err
	}
	return GatewayCredentialRotation{Owner: owner, Generation: generation, ExpiresAt: expiresAt}, nil
}

func (s *Store) RenewGatewayCredentialRotation(ctx context.Context, rotation GatewayCredentialRotation, now time.Time) error {
	if rotation.Owner == "" || rotation.Generation <= 0 {
		return ErrFencingConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE gateway_runtime SET rotation_expires_at = ?, updated_at = ? WHERE singleton = 1 AND rotation_owner = ? AND rotation_generation = ? AND rotation_expires_at > ?`, formatTimestamp(now.Add(gatewayCredentialRotationLeaseTTL)), formatTimestamp(now), rotation.Owner, rotation.Generation, formatTimestamp(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) EndGatewayCredentialRotation(ctx context.Context, rotation GatewayCredentialRotation, now time.Time) error {
	if rotation.Owner == "" || rotation.Generation <= 0 {
		return ErrFencingConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE gateway_runtime SET rotation_owner = '', rotation_expires_at = '', updated_at = ? WHERE singleton = 1 AND rotation_owner = ? AND rotation_generation = ?`, formatTimestamp(now), rotation.Owner, rotation.Generation)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) ResumeGatewayWrites(ctx context.Context, rotation GatewayCredentialRotation, now time.Time) error {
	if rotation.Owner == "" || rotation.Generation <= 0 {
		return ErrFencingConflict
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	repositories, err := workflowQuestionRepositoriesTx(ctx, tx, gatewayCredentialQuestionKind)
	if err != nil {
		return err
	}
	timestamp := now.Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE gateway_runtime SET writes_paused = 0, reason = '', rotation_owner = '', rotation_expires_at = '', updated_at = ? WHERE singleton = 1 AND rotation_owner = ? AND rotation_generation = ? AND rotation_expires_at > ?`, timestamp, rotation.Owner, rotation.Generation, timestamp)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrFencingConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_inbox SET state = 'resolved', updated_at = ? WHERE item_key = ?`, timestamp, GatewayCredentialInboxKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_questions SET state = 'answered', answer = 'credential restored', answered_at = ? WHERE kind = ? AND state = 'open'`, timestamp, gatewayCredentialQuestionKind); err != nil {
		return err
	}
	for _, repository := range repositories {
		if _, err := s.queueWorkflowInboxProjectionIfActiveTx(ctx, tx, repository, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func workflowQuestionRepositoriesTx(ctx context.Context, tx *sql.Tx, kind string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT repository FROM workflow_questions WHERE kind = ? AND state = 'open' ORDER BY repository`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []string
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

func ensureGatewayCredentialQuestionsTx(ctx context.Context, tx *sql.Tx, reason string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT p.repository, v.version_id
FROM plans p JOIN plan_versions v ON v.version_id = p.current_version_id
WHERE `+currentActivePlanPredicate)
	if err != nil {
		return err
	}
	var plans []struct {
		repository string
		versionID  string
	}
	for rows.Next() {
		var repository, versionID string
		if err := rows.Scan(&repository, &versionID); err != nil {
			rows.Close()
			return err
		}
		plans = append(plans, struct {
			repository string
			versionID  string
		}{repository: repository, versionID: versionID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, plan := range plans {
		if err := ensureWorkflowQuestionTx(ctx, tx, plan.repository, plan.versionID, 0, gatewayCredentialQuestionKind, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GatewayCredentialAttentionRepositories(ctx context.Context) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	repositories, err := workflowQuestionRepositoriesTx(ctx, tx, gatewayCredentialQuestionKind)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return repositories, nil
}

func (s *Store) GatewayWritesPaused(ctx context.Context) (bool, string, error) {
	var paused int
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT writes_paused, reason FROM gateway_runtime WHERE singleton = 1`).Scan(&paused, &reason)
	return paused != 0, reason, err
}

func (s *Store) WaitForGatewayWritesQuiesced(ctx context.Context) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		paused, _, err := s.GatewayWritesPaused(ctx)
		if err != nil {
			return err
		}
		if paused {
			if err := s.RecoverExpiredGatewayDeliveryClaims(ctx, time.Now().UTC()); err != nil {
				return err
			}
		}
		var processing int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_outbox WHERE state = 'processing'`).Scan(&processing); err != nil {
			return err
		}
		if processing == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Store) EnsureGatewayDispatcher(ctx context.Context, dispatcherToken string, now time.Time) error {
	if dispatcherToken == "" {
		return errors.New("Gateway dispatcher token is required")
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
		return err
	}
	defer tx.Rollback()
	var activeToken, expiresText string
	if err := tx.QueryRowContext(ctx, `SELECT dispatcher_token, dispatcher_expires_at FROM gateway_runtime WHERE singleton = 1`).Scan(&activeToken, &expiresText); err != nil {
		return err
	}
	if activeToken != "" && activeToken != dispatcherToken && expiresText != "" {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresText)
		if parseErr != nil {
			return parseErr
		}
		if expiresAt.After(now) {
			return ErrDeliveryInProgress
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE gateway_runtime SET dispatcher_token = ?, dispatcher_expires_at = ?, updated_at = ? WHERE singleton = 1`, dispatcherToken, formatTimestamp(now.Add(gatewayDispatcherLeaseTTL)), formatTimestamp(now))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RenewGatewayDispatcher(ctx context.Context, dispatcherToken string, now time.Time) error {
	if dispatcherToken == "" {
		return errors.New("Gateway dispatcher token is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE gateway_runtime SET dispatcher_expires_at = ?, updated_at = ? WHERE singleton = 1 AND dispatcher_token = ? AND dispatcher_expires_at > ?`, formatTimestamp(now.Add(gatewayDispatcherLeaseTTL)), formatTimestamp(now), dispatcherToken, formatTimestamp(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrFencingConflict
	}
	return nil
}

func (s *Store) RecoverExpiredGatewayDeliveryClaims(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var writesPaused int
	if err := tx.QueryRowContext(ctx, `SELECT writes_paused FROM gateway_runtime WHERE singleton = 1`).Scan(&writesPaused); err != nil {
		return err
	}
	if writesPaused == 0 {
		return errors.New("Gateway writes must be paused before recovering delivery claims")
	}
	rows, err := tx.QueryContext(ctx, `SELECT idempotency_key, request_json, updated_at, claim_token, dispatcher_token FROM delivery_outbox WHERE state = ?`, OutboxProcessing)
	if err != nil {
		return err
	}
	type expiredClaim struct {
		key             string
		claimToken      string
		dispatcherToken string
	}
	var expired []expiredClaim
	for rows.Next() {
		var key, raw, updatedAt, claimToken string
		var dispatcherToken string
		if err := rows.Scan(&key, &raw, &updatedAt, &claimToken, &dispatcherToken); err != nil {
			rows.Close()
			return err
		}
		recoverable, recoverErr := deliveryOutboxClaimRecoverableTx(ctx, tx, raw, updatedAt, dispatcherToken, now)
		if recoverErr != nil {
			rows.Close()
			return recoverErr
		}
		if recoverable {
			expired = append(expired, expiredClaim{key: key, claimToken: claimToken, dispatcherToken: dispatcherToken})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, claim := range expired {
		result, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, claim_token = '', dispatcher_token = '', uncertain = 1, last_error = ?, next_attempt_at = ?, updated_at = ? WHERE idempotency_key = ? AND state = ? AND claim_token = ? AND dispatcher_token = ?`, OutboxPending, "delivery claim expired while Gateway writes were paused", formatTimestamp(now), formatTimestamp(now), claim.key, OutboxProcessing, claim.claimToken, claim.dispatcherToken)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return ErrFencingConflict
		}
	}
	return tx.Commit()
}

func (s *Store) WorkflowInboxItem(ctx context.Context, key string) (WorkflowInboxItem, error) {
	var item WorkflowInboxItem
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `SELECT item_key, kind, title, body, state, created_at, updated_at FROM workflow_inbox WHERE item_key = ?`, key).
		Scan(&item.Key, &item.Kind, &item.Title, &item.Body, &item.State, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowInboxItem{}, ErrNotFound
	}
	if err != nil {
		return WorkflowInboxItem{}, err
	}
	item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return WorkflowInboxItem{}, err
	}
	item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	return item, err
}
