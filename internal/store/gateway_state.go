package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const GatewayCredentialInboxKey = "gateway-credential"

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
	if reason == "" {
		return errors.New("Gateway pause reason is required")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	return tx.Commit()
}

func (s *Store) ResumeGatewayWrites(ctx context.Context, now time.Time) error {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	timestamp := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_runtime SET writes_paused = 0, reason = '', updated_at = ? WHERE singleton = 1`, timestamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_inbox SET state = 'resolved', updated_at = ? WHERE item_key = ?`, timestamp, GatewayCredentialInboxKey); err != nil {
		return err
	}
	return tx.Commit()
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
