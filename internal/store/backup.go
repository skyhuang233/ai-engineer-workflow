package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const backupMetadataSuffix = ".metadata.json"

// BackupMetadata is the transportable provenance for an online Control Plane
// backup. The same reference records are persisted in SQLite for audit.
type BackupMetadata struct {
	BackupPath          string            `json:"backup_path"`
	CreatedAt           time.Time         `json:"created_at"`
	ChecksumSHA256      string            `json:"checksum_sha256"`
	SchemaVersion       int               `json:"schema_version"`
	WorkspaceReferences []BackupReference `json:"workspace_references"`
	ArtifactReferences  []BackupReference `json:"artifact_references"`
	LastDrill           BackupDrill       `json:"last_drill"`
}

// BackupReference binds an external workspace or artifact path to the bytes
// observed during backup. Unavailable paths remain auditable without making a
// database backup unusable during host migration.
type BackupReference struct {
	Path           string `json:"path"`
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
	Available      bool   `json:"available"`
}

type BackupDrill struct {
	At        time.Time        `json:"at,omitempty"`
	Succeeded bool             `json:"succeeded"`
	Error     string           `json:"error,omitempty"`
	Reconcile ReconcilePreview `json:"reconcile"`
}

// ReconcilePreview reports the durable work that a local recovery pass will
// converge. It intentionally performs no GitHub I/O.
type ReconcilePreview struct {
	ActiveSessions   int `json:"active_sessions"`
	ActiveLeases     int `json:"active_leases"`
	ProcessingOutbox int `json:"processing_outbox"`
	PollCursors      int `json:"poll_cursors"`
	OpenInbox        int `json:"open_inbox"`
}

type OperationalMetrics struct {
	BackupAge      time.Duration `json:"backup_age"`
	DrillSucceeded bool          `json:"drill_succeeded"`
	OutboxAge      time.Duration `json:"outbox_age"`
	ReconcileLag   time.Duration `json:"reconcile_lag"`
}

type onlineBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

type onlineRestorer interface {
	NewRestore(string) (*sqlite.Backup, error)
}

func backupMetadataPath(backupPath string) string { return backupPath + backupMetadataSuffix }

// CreateOnlineBackup snapshots SQLite through its online backup API before
// publishing a checksum-verified backup and its provenance sidecar.
func (s *Store) CreateOnlineBackup(ctx context.Context, backupPath string, now time.Time) (BackupMetadata, error) {
	if s == nil || s.db == nil || s.databasePath == "" || strings.TrimSpace(backupPath) == "" {
		return BackupMetadata{}, ErrInvalidClaim
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
		return BackupMetadata{}, fmt.Errorf("create SQLite backup directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(backupPath), filepath.Base(backupPath)+".online-*.tmp")
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("create SQLite backup temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return BackupMetadata{}, fmt.Errorf("close SQLite backup temporary file: %w", err)
	}
	defer os.Remove(temporaryPath)
	if err := copyOnline(ctx, s.db, temporaryPath); err != nil {
		return BackupMetadata{}, err
	}
	if err := verifySQLite(ctx, temporaryPath); err != nil {
		return BackupMetadata{}, fmt.Errorf("verify SQLite online backup: %w", err)
	}
	checksum, err := checksumFile(temporaryPath)
	if err != nil {
		return BackupMetadata{}, err
	}
	schemaVersion, err := schemaVersionAt(ctx, temporaryPath)
	if err != nil {
		return BackupMetadata{}, err
	}
	references, err := s.backupReferences(ctx)
	if err != nil {
		return BackupMetadata{}, err
	}
	metadata := BackupMetadata{
		BackupPath: backupPath, CreatedAt: now, ChecksumSHA256: checksum, SchemaVersion: schemaVersion,
		WorkspaceReferences: references.workspaces, ArtifactReferences: references.artifacts,
	}
	if err := replaceFile(temporaryPath, backupPath); err != nil {
		return BackupMetadata{}, fmt.Errorf("publish SQLite online backup: %w", err)
	}
	if err := writeBackupMetadata(backupPath, metadata); err != nil {
		return BackupMetadata{}, err
	}
	if err := s.recordBackupProvenance(ctx, metadata); err != nil {
		return BackupMetadata{}, err
	}
	if _, err := DrillBackup(ctx, backupPath, now); err != nil {
		return BackupMetadata{}, fmt.Errorf("drill SQLite online backup: %w", err)
	}
	metadata, err = LoadBackupMetadata(backupPath)
	if err != nil {
		return BackupMetadata{}, err
	}
	if err := s.recordBackupProvenance(ctx, metadata); err != nil {
		metadata.LastDrill = BackupDrill{At: now, Error: "record SQLite backup provenance: " + err.Error()}
		_ = writeBackupMetadata(backupPath, metadata)
		return BackupMetadata{}, err
	}
	return metadata, nil
}

type backupReferences struct{ workspaces, artifacts []BackupReference }

func (s *Store) backupReferences(ctx context.Context) (backupReferences, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workspace_path, codex_state_path FROM ticket_sessions WHERE workspace_path != '' OR codex_state_path != ''`)
	if err != nil {
		return backupReferences{}, err
	}
	defer rows.Close()
	var references backupReferences
	for rows.Next() {
		var workspace, codex string
		if err := rows.Scan(&workspace, &codex); err != nil {
			return backupReferences{}, err
		}
		references.workspaces = append(references.workspaces, referenceForPath(workspace), referenceForPath(codex))
	}
	if err := rows.Err(); err != nil {
		return backupReferences{}, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT diagnostics_path FROM run_diagnostics WHERE diagnostics_path != ''`)
	if err != nil {
		return backupReferences{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifact string
		if err := rows.Scan(&artifact); err != nil {
			return backupReferences{}, err
		}
		references.artifacts = append(references.artifacts, referenceForPath(artifact))
	}
	if err := rows.Err(); err != nil {
		return backupReferences{}, err
	}
	references.workspaces, err = checksumReferences(references.workspaces)
	if err != nil {
		return backupReferences{}, err
	}
	references.artifacts, err = checksumReferences(references.artifacts)
	if err != nil {
		return backupReferences{}, err
	}
	return references, nil
}

func referenceForPath(path string) BackupReference {
	return BackupReference{Path: strings.TrimSpace(path)}
}

func checksumReferences(values []BackupReference) ([]BackupReference, error) {
	unique := make(map[string]BackupReference, len(values))
	for _, value := range values {
		if value.Path != "" {
			unique[value.Path] = value
		}
	}
	result := make([]BackupReference, 0, len(unique))
	for _, value := range unique {
		checksum, available, err := checksumReference(value.Path)
		if err != nil {
			return nil, err
		}
		value.ChecksumSHA256, value.Available = checksum, available
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func checksumReference(path string) (string, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat backup reference %q: %w", path, err)
	}
	if !info.IsDir() {
		checksum, err := checksumFile(path)
		return checksum, true, err
	}
	hash := sha256.New()
	err = filepath.Walk(path, func(file string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		// Active Ticket Session directories can contain transient sockets and
		// other non-transportable entries; provenance covers regular file bytes.
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(path, file)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		_, _ = hash.Write([]byte(relative))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(contents)
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("hash backup reference %q: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), true, nil
}

func (s *Store) recordBackupProvenance(ctx context.Context, metadata BackupMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_backups(backup_path, checksum_sha256, schema_version, metadata_json, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(backup_path) DO UPDATE SET checksum_sha256 = excluded.checksum_sha256, schema_version = excluded.schema_version, metadata_json = excluded.metadata_json, created_at = excluded.created_at`, metadata.BackupPath, metadata.ChecksumSHA256, metadata.SchemaVersion, string(encoded), formatTimestamp(metadata.CreatedAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM control_plane_backup_references WHERE backup_path = ?`, metadata.BackupPath); err != nil {
		return err
	}
	for _, record := range []struct {
		kind string
		refs []BackupReference
	}{{"workspace", metadata.WorkspaceReferences}, {"artifact", metadata.ArtifactReferences}} {
		for _, reference := range record.refs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_backup_references(backup_path, kind, reference_path, checksum_sha256, available) VALUES (?, ?, ?, ?, ?)`, metadata.BackupPath, record.kind, reference.Path, reference.ChecksumSHA256, boolInt(reference.Available)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func copyOnline(ctx context.Context, source *sql.DB, destination string) error {
	conn, err := source.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire SQLite source connection: %w", err)
	}
	defer conn.Close()
	return conn.Raw(func(raw any) error {
		backuper, ok := raw.(onlineBackuper)
		if !ok {
			return errors.New("SQLite driver does not expose online backup API")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return fmt.Errorf("start SQLite online backup: %w", err)
		}
		_, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		return errors.Join(stepErr, finishErr)
	})
}

func restoreOnline(ctx context.Context, source, destination string) error {
	db, err := sql.Open("sqlite", destination)
	if err != nil {
		return fmt.Errorf("open restore destination: %w", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire restore destination: %w", err)
	}
	defer conn.Close()
	return conn.Raw(func(raw any) error {
		restorer, ok := raw.(onlineRestorer)
		if !ok {
			return errors.New("SQLite driver does not expose online restore API")
		}
		backup, err := restorer.NewRestore(source)
		if err != nil {
			return fmt.Errorf("start SQLite online restore: %w", err)
		}
		_, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		return errors.Join(stepErr, finishErr)
	})
}

// RestoreBackup verifies the immutable backup, restores it with SQLite's
// online restore API, then runs the normal integrity and migration boundary.
func RestoreBackup(ctx context.Context, backupPath, destination string) error {
	metadata, err := LoadBackupMetadata(backupPath)
	if err != nil {
		return err
	}
	if err := verifyBackup(ctx, backupPath, metadata); err != nil {
		return err
	}
	if strings.TrimSpace(destination) == "" {
		return ErrInvalidClaim
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), filepath.Base(destination)+".restore-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return err
	}
	defer os.Remove(temporaryPath)
	if err := restoreOnline(ctx, backupPath, temporaryPath); err != nil {
		return err
	}
	if err := verifySQLite(ctx, temporaryPath); err != nil {
		return fmt.Errorf("verify restored SQLite database: %w", err)
	}
	restored, err := Open(ctx, temporaryPath)
	if err != nil {
		return fmt.Errorf("migrate restored SQLite database: %w", err)
	}
	if err := restored.Close(); err != nil {
		return err
	}
	if err := verifySQLite(ctx, temporaryPath); err != nil {
		return fmt.Errorf("verify migrated SQLite database: %w", err)
	}
	return replaceDatabaseFile(temporaryPath, destination)
}

// DrillBackup proves a backup can be restored and migrated in a disposable
// location. Its reconcile preview never calls a GitHub client or dispatcher.
func DrillBackup(ctx context.Context, backupPath string, now time.Time) (BackupDrill, error) {
	metadata, err := LoadBackupMetadata(backupPath)
	if err != nil {
		return BackupDrill{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	fail := func(cause error) (BackupDrill, error) {
		metadata.LastDrill = BackupDrill{At: now, Error: cause.Error()}
		return BackupDrill{}, errors.Join(cause, writeBackupMetadata(backupPath, metadata))
	}
	if err := verifyBackup(ctx, backupPath, metadata); err != nil {
		return fail(err)
	}
	directory, err := os.MkdirTemp(filepath.Dir(backupPath), "workflow-backup-drill-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(directory)
	restoredPath := filepath.Join(directory, "restored.db")
	if err := RestoreBackup(ctx, backupPath, restoredPath); err != nil {
		return fail(err)
	}
	restored, err := Open(ctx, restoredPath)
	if err != nil {
		return fail(err)
	}
	if err := restored.ReconcileRestoredControlPlaneDryRun(ctx, now); err != nil {
		restored.Close()
		return fail(err)
	}
	preview, previewErr := restored.ReconcilePreview(ctx)
	closeErr := restored.Close()
	if err := errors.Join(previewErr, closeErr); err != nil {
		return fail(err)
	}
	drill := BackupDrill{At: now, Succeeded: true, Reconcile: preview}
	metadata.LastDrill = drill
	if err := writeBackupMetadata(backupPath, metadata); err != nil {
		return BackupDrill{}, err
	}
	return drill, nil
}

func LoadBackupMetadata(backupPath string) (BackupMetadata, error) {
	data, err := os.ReadFile(backupMetadataPath(backupPath))
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("read SQLite backup metadata: %w", err)
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return BackupMetadata{}, fmt.Errorf("decode SQLite backup metadata: %w", err)
	}
	if metadata.ChecksumSHA256 == "" || metadata.SchemaVersion <= 0 {
		return BackupMetadata{}, errors.New("SQLite backup metadata is incomplete")
	}
	return metadata, nil
}

func writeBackupMetadata(backupPath string, metadata BackupMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(backupPath), filepath.Base(backupPath)+".metadata-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(temporaryPath, backupMetadataPath(backupPath))
}

func verifyBackupChecksum(path string, metadata BackupMetadata) error {
	checksum, err := checksumFile(path)
	if err != nil {
		return err
	}
	if checksum != metadata.ChecksumSHA256 {
		return fmt.Errorf("SQLite backup checksum mismatch: got %s", checksum)
	}
	return nil
}

func verifyBackup(ctx context.Context, path string, metadata BackupMetadata) error {
	if err := verifyBackupChecksum(path, metadata); err != nil {
		return err
	}
	version, err := schemaVersionAt(ctx, path)
	if err != nil {
		return fmt.Errorf("read SQLite backup schema version: %w", err)
	}
	if version != metadata.SchemaVersion {
		return fmt.Errorf("SQLite backup schema version mismatch: got %d want %d", version, metadata.SchemaVersion)
	}
	return nil
}

func checksumFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read SQLite backup: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func verifySQLite(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity_check = %q", integrity)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign_key_check found a violation")
	}
	return rows.Err()
}

func schemaVersionAt(ctx context.Context, path string) (int, error) {
	db, err := OpenForStartup(ctx, path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return db.schemaVersion(ctx)
}

func replaceFile(temporary, destination string) error {
	archived := ""
	if _, err := os.Stat(destination); err == nil {
		archived = fmt.Sprintf("%s.previous.%d", destination, time.Now().UTC().UnixNano())
		if err := os.Rename(destination, archived); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		if archived != "" {
			_ = os.Rename(archived, destination)
		}
		return err
	}
	if archived != "" {
		_ = os.Remove(archived)
	}
	return nil
}

func replaceDatabaseFile(temporary, destination string) error {
	archiveBase := fmt.Sprintf("%s.pre-restore.%d", destination, time.Now().UTC().UnixNano())
	targets := []string{destination, destination + "-wal", destination + "-shm"}
	archived := make([]struct{ source, destination string }, 0, len(targets))
	for _, target := range targets {
		if _, err := os.Stat(target); err == nil {
			archive := archiveBase + strings.TrimPrefix(target, destination)
			if err := os.Rename(target, archive); err != nil {
				for index := len(archived) - 1; index >= 0; index-- {
					_ = os.Rename(archived[index].destination, archived[index].source)
				}
				return err
			}
			archived = append(archived, struct{ source, destination string }{target, archive})
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(temporary, destination); err != nil {
		for index := len(archived) - 1; index >= 0; index-- {
			_ = os.Rename(archived[index].destination, archived[index].source)
		}
		return err
	}
	for _, archive := range archived {
		_ = os.Remove(archive.destination)
	}
	return nil
}

func (s *Store) ReconcilePreview(ctx context.Context) (ReconcilePreview, error) {
	var preview ReconcilePreview
	for _, count := range []struct {
		query string
		into  *int
	}{
		{`SELECT COUNT(*) FROM ticket_sessions WHERE state = 'running'`, &preview.ActiveSessions},
		{`SELECT COUNT(*) FROM run_leases WHERE state = 'active'`, &preview.ActiveLeases},
		{`SELECT COUNT(*) FROM delivery_outbox WHERE state = 'processing'`, &preview.ProcessingOutbox},
		{`SELECT COUNT(*) FROM github_poll_cursors`, &preview.PollCursors},
		{`SELECT COUNT(*) FROM workflow_questions WHERE state = 'open'`, &preview.OpenInbox},
	} {
		if err := s.db.QueryRowContext(ctx, count.query).Scan(count.into); err != nil {
			return ReconcilePreview{}, err
		}
	}
	return preview, nil
}

// ReconcileRestoredControlPlane applies only local, idempotent recovery. It
// turns unknown outbox writes into reconcile-only work, releases Worker Runs
// with a fresh lease boundary, preserves Inbox questions, and makes pollers
// eligible to re-observe GitHub without issuing a write itself.
func (s *Store) ReconcileRestoredControlPlane(ctx context.Context, now time.Time) error {
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
	if err := reconcileRestoredControlPlaneTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileRestoredControlPlaneDryRun exercises the same local convergence in
// a rollback-only transaction. It is used by backup drills and has no remote
// side effects.
func (s *Store) ReconcileRestoredControlPlaneDryRun(ctx context.Context, now time.Time) error {
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
	return reconcileRestoredControlPlaneTx(ctx, tx, now)
}

func reconcileRestoredControlPlaneTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	stamp := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_outbox SET state = ?, claim_token = '', dispatcher_token = '', uncertain = 1, last_error = ?, next_attempt_at = ?, updated_at = ? WHERE state = ?`, OutboxPending, "delivery state was interrupted by Control Plane restore", stamp, stamp, OutboxProcessing); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE github_poll_cursors SET last_success_at = '', last_full_reconcile_at = '', next_attempt_at = ?, updated_at = ?`, stamp, stamp); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.run_id, r.run_kind, s.version_id, s.issue_id, l.lease_token
FROM worker_runs r JOIN ticket_sessions s ON s.current_run_id = r.run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
WHERE r.state = ? AND l.state = ?`, RunRunning, LeaseActive)
	if err != nil {
		return err
	}
	type activeRun struct {
		runID, kind, versionID, lease string
		issueID                       int64
	}
	var active []activeRun
	for rows.Next() {
		var run activeRun
		if err := rows.Scan(&run.runID, &run.kind, &run.versionID, &run.issueID, &run.lease); err != nil {
			rows.Close()
			return err
		}
		active = append(active, run)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, run := range active {
		if run.kind == RunDelivery {
			if err := markTicketNeedsAttentionTx(ctx, tx, run.versionID, run.issueID, "Delivery Controller was interrupted by Control Plane restore", now); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'superseded', finished_at = ? WHERE run_id = ? AND state = ?`, stamp, run.runID, RunRunning); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'expired' WHERE run_id = ? AND lease_token = ? AND state = ?`, run.runID, run.lease, LeaseActive); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_feedback_events SET claimed_run_id = '' WHERE claimed_run_id = ?`, run.runID); err != nil {
			return err
		}
		if _, err := releaseMergeReadyRevalidationsTx(ctx, tx, run.runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = CASE WHEN EXISTS (
    SELECT 1 FROM workflow_questions q WHERE q.version_id = ticket_runtime.version_id AND q.issue_id = ticket_runtime.issue_id AND q.state = 'open'
) THEN 'needs_attention' ELSE 'queued' END, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, stamp, run.versionID, run.issueID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) OperationalMetrics(ctx context.Context, backupPath string, now time.Time) (OperationalMetrics, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	metadata, err := LoadBackupMetadata(backupPath)
	if err != nil {
		return OperationalMetrics{}, err
	}
	if err := verifyBackup(ctx, backupPath, metadata); err != nil {
		return OperationalMetrics{}, err
	}
	metrics := OperationalMetrics{BackupAge: now.Sub(metadata.CreatedAt), DrillSucceeded: metadata.LastDrill.Succeeded}
	var oldestOutbox, oldestReconcile string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(created_at), '') FROM delivery_outbox WHERE state IN ('pending', 'processing')`).Scan(&oldestOutbox); err != nil {
		return OperationalMetrics{}, err
	}
	if oldestOutbox != "" {
		value, err := time.Parse(time.RFC3339Nano, oldestOutbox)
		if err != nil {
			return OperationalMetrics{}, err
		}
		metrics.OutboxAge = now.Sub(value)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(last_full_reconcile_at), '') FROM github_poll_cursors WHERE last_full_reconcile_at != ''`).Scan(&oldestReconcile); err != nil {
		return OperationalMetrics{}, err
	}
	if oldestReconcile != "" {
		value, err := time.Parse(time.RFC3339Nano, oldestReconcile)
		if err != nil {
			return OperationalMetrics{}, err
		}
		metrics.ReconcileLag = now.Sub(value)
	}
	return metrics, nil
}
