package store

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

func TestOnlineBackupRestoreDrillAndOperationalMetrics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	databasePath := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := plan.Snapshot{
		Repository: "owner/repository",
		Root:       plan.Issue{ID: 1, Number: 1, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 2, Number: 2, Title: "active ticket", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "root-revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	if runtime.GOOS == "windows" {
		workspaceRoot, err = os.MkdirTemp(filepath.VolumeName(os.TempDir())+string(os.PathSeparator), "wf-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(workspaceRoot) })
	}
	workspace := filepath.Join(workspaceRoot, "workspace")
	diagnostics := filepath.Join(t.TempDir(), "artifacts", "run.log")
	if err := os.MkdirAll(filepath.Join(workspace, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(workspace, ".codex", "no-mistakes", "socket")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	socket, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := os.MkdirAll(filepath.Dir(diagnostics), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diagnostics, []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_sessions SET workspace_path = ?, codex_state_path = ? WHERE session_id = ?`, workspace, filepath.Join(workspace, ".codex"), claim.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO run_diagnostics(run_id, diagnostics_path, error, created_at) VALUES (?, ?, ?, ?)`, claim.RunID, diagnostics, "interrupted", formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordGitHubPollSuccess(ctx, snapshot.Repository, now.Add(-time.Hour), true); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTicketNeedsAttentionTx(ctx, tx, version.ID, claim.TicketID, "operator question must survive restore", now.Add(-40*time.Minute)); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	outbox, err := db.QueueWorkflowInboxProjection(ctx, snapshot.Repository, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	processing, err := db.ClaimDeliveryOutbox(ctx, outbox.IdempotencyKey, now.Add(-20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(t.TempDir(), "workflow.backup.db")
	metadata, err := db.CreateOnlineBackup(ctx, backupPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SchemaVersion != latestSchemaVersion || metadata.ChecksumSHA256 == "" || !metadata.LastDrill.Succeeded || len(metadata.WorkspaceReferences) != 2 || len(metadata.ArtifactReferences) != 1 || !metadata.WorkspaceReferences[0].Available || metadata.WorkspaceReferences[0].ChecksumSHA256 == "" || !metadata.ArtifactReferences[0].Available || metadata.ArtifactReferences[0].ChecksumSHA256 == "" {
		t.Fatalf("backup metadata = %#v", metadata)
	}
	var persistedReferences int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_plane_backup_references WHERE backup_path = ? AND available = 1`, backupPath).Scan(&persistedReferences); err != nil || persistedReferences != 3 {
		t.Fatalf("persisted backup references = %d, %v", persistedReferences, err)
	}
	drill, err := DrillBackup(ctx, backupPath, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !drill.Succeeded || drill.Reconcile.ActiveSessions != 1 || drill.Reconcile.ProcessingOutbox != 1 || drill.Reconcile.PollCursors != 1 || drill.Reconcile.OpenInbox != 1 {
		t.Fatalf("drill = %#v", drill)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreBackup(ctx, backupPath, restoredPath); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.ReconcileRestoredControlPlane(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recovered, err := restored.DeliveryOutbox(ctx, outbox.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != OutboxPending || !recovered.Uncertain || recovered.ClaimToken != "" {
		t.Fatalf("recovered outbox = %#v, want uncertain pending", recovered)
	}
	cursor, err := restored.GitHubPollCursor(ctx, snapshot.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.NextAttemptAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("poll cursor next attempt = %s", cursor.NextAttemptAt)
	}
	if !cursor.LastSuccessAt.IsZero() || !cursor.LastFullReconcileAt.IsZero() {
		t.Fatalf("restored poll cursor retained incremental boundary: %#v", cursor)
	}
	preview, err := restored.ReconcilePreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ActiveLeases != 0 || preview.OpenInbox != 1 {
		t.Fatalf("restored recovery preview = %#v", preview)
	}
	metrics, err := restored.OperationalMetrics(ctx, backupPath, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if metrics.BackupAge != 3*time.Minute || metrics.OutboxAge <= 0 || metrics.ReconcileLag != 0 {

		t.Fatalf("operational metrics = %#v", metrics)
	}
	if err := os.WriteFile(backupPath, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DrillBackup(ctx, backupPath, now.Add(4*time.Minute)); err == nil {
		t.Fatal("corrupt backup passed restore drill")
	}
	failedMetadata, err := LoadBackupMetadata(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if failedMetadata.LastDrill.Succeeded || failedMetadata.LastDrill.Error == "" {
		t.Fatalf("failed backup drill was marked successful: %#v", failedMetadata.LastDrill)
	}

	_ = processing
}
