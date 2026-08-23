package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type SQLiteHealth struct {
	JournalMode  string
	Synchronous  int
	ForeignKeys  bool
	Integrity    string
	WriteLocking bool
}

func (s *Store) Health(ctx context.Context) (SQLiteHealth, error) {
	var health SQLiteHealth
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&health.JournalMode); err != nil {
		return SQLiteHealth{}, fmt.Errorf("read journal_mode: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&health.Synchronous); err != nil {
		return SQLiteHealth{}, fmt.Errorf("read synchronous: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return SQLiteHealth{}, fmt.Errorf("read foreign_keys: %w", err)
	}
	health.ForeignKeys = foreignKeys == 1
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&health.Integrity); err != nil {
		return SQLiteHealth{}, fmt.Errorf("run integrity_check: %w", err)
	}
	if s.databasePath == "" {
		return health, nil
	}
	locked, err := probeWriteLock(ctx, s.databasePath)
	if err != nil {
		return SQLiteHealth{}, err
	}
	health.WriteLocking = locked
	return health, nil
}

func probeWriteLock(ctx context.Context, path string) (bool, error) {
	first, err := sql.Open("sqlite", path)
	if err != nil {
		return false, err
	}
	defer first.Close()
	second, err := sql.Open("sqlite", path)
	if err != nil {
		return false, err
	}
	defer second.Close()
	for _, db := range []*sql.DB{first, second} {
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 50"); err != nil {
			return false, err
		}
	}
	conn, err := first.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, fmt.Errorf("acquire first SQLite write lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	_, lockErr := second.ExecContext(ctx, "BEGIN IMMEDIATE")
	if lockErr == nil {
		_, _ = second.ExecContext(context.Background(), "ROLLBACK")
		return false, nil
	}
	message := strings.ToLower(lockErr.Error())
	if strings.Contains(message, "locked") || strings.Contains(message, "busy") {
		return true, nil
	}
	return false, errors.New("second SQLite writer failed for an unexpected reason: " + lockErr.Error())
}
