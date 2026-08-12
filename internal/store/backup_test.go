package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	sessionState := []byte(`{"thread":"ticket-2"}`)
	sessionStatePath := filepath.Join(workspace, ".codex", "session.json")
	if err := os.WriteFile(sessionStatePath, sessionState, 0o600); err != nil {
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
	workspaceHash := sha256.New()
	_, _ = workspaceHash.Write([]byte(filepath.Join(".codex", "session.json")))
	_, _ = workspaceHash.Write([]byte{0})
	_, _ = workspaceHash.Write(sessionState)
	wantWorkspaceChecksum := hex.EncodeToString(workspaceHash.Sum(nil))
	if metadata.WorkspaceReferences[0].Path != workspace || metadata.WorkspaceReferences[0].ChecksumSHA256 != wantWorkspaceChecksum {
		t.Fatalf("workspace checksum = %#v, want regular Ticket Session file checksum %s", metadata.WorkspaceReferences[0], wantWorkspaceChecksum)
	}
	t.Logf("online backup accepted an active AF_UNIX socket and hashed the regular Ticket Session file: workspace_checksum=%s automatic_restore_drill=%t", metadata.WorkspaceReferences[0].ChecksumSHA256, metadata.LastDrill.Succeeded)
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
	t.Logf("isolated restore reconciled durable state: active_sessions=%d active_leases=%d processing_outbox=%d poll_cursors=%d open_inbox=%d", preview.ActiveSessions, preview.ActiveLeases, preview.ProcessingOutbox, preview.PollCursors, preview.OpenInbox)
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

func TestRestoreBackupMigratesLegacySchemaWithoutDeliverySourceProvenance(t *testing.T) {
	ctx := context.Background()
	backupPath := filepath.Join(t.TempDir(), "workflow-v50.db")
	db, err := Open(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "ALTER TABLE candidate_revisions DROP COLUMN delivery_source_digest"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= ?", deliverySourceBackupSchemaVersion); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checksum, err := checksumFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := BackupMetadata{BackupPath: backupPath, ChecksumSHA256: checksum, SchemaVersion: deliverySourceBackupSchemaVersion - 1}
	if err := writeBackupMetadata(backupPath, metadata); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreBackup(ctx, backupPath, restoredPath); err != nil {
		t.Fatalf("restore schema 50 backup: %v", err)
	}
	restored, err := OpenForStartup(ctx, restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	version, err := restored.schemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != latestSchemaVersion {
		t.Fatalf("restored schema version = %d, want %d", version, latestSchemaVersion)
	}
	if !hasColumn(t, ctx, restored.db, "candidate_revisions", "delivery_source_digest") {
		t.Fatal("restored Delivery Source column is missing")
	}
}

func TestBackupDrillRequiresCurrentCandidateDeliverySource(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	workspace := filepath.Join(workspaceRoot, claim.SessionID)
	deliverySource := filepath.Join(workspaceRoot, ".delivery-sources", claim.SessionID, "round-0.git")
	if err := os.MkdirAll(deliverySource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deliverySource, "snapshot"), []byte("pinned source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE ticket_sessions SET workspace_path = ? WHERE session_id = ?`, workspace, claim.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), DeliverySourceDigest: strings.Repeat("a", 64), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "workflow.backup.db")
	metadata, err := db.CreateOnlineBackup(ctx, backupPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.DeliverySourceReferences) != 1 || metadata.DeliverySourceReferences[0].Path != deliverySource || !metadata.DeliverySourceReferences[0].Available || metadata.DeliverySourceReferences[0].ChecksumSHA256 == "" {
		t.Fatalf("Delivery Source backup provenance = %#v", metadata.DeliverySourceReferences)
	}
	if err := os.RemoveAll(deliverySource); err != nil {
		t.Fatal(err)
	}
	if _, err := DrillBackup(ctx, backupPath, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "Delivery Source backup reference") {
		t.Fatalf("missing Delivery Source drill error = %v", err)
	}
}

func TestBackupAndRestorePreparedDeliveryRequireRealIsolationOnlyOnApply(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := testSnapshot()
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveDeliveryControllerPrelaunch(ctx, delivery, now); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "workflow.backup.db")
	metadata, err := db.CreateOnlineBackup(ctx, backupPath, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.LastDrill.Succeeded {
		t.Fatalf("prepared-delivery restore drill = %#v", metadata.LastDrill)
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
	err = restored.ReconcileRestoredControlPlane(ctx, now.Add(2*time.Minute))
	var isolation *WorkerIsolationRequired
	if !errors.As(err, &isolation) || len(isolation.Targets) != 1 || isolation.Targets[0].RunID != delivery.RunID {
		t.Fatalf("restore isolation requirement = %#v, %v", isolation, err)
	}
	fenced, err := restored.FenceWorkerIsolation(ctx, isolation.Targets)
	if err != nil {
		t.Fatal(err)
	}
	proofs, err := restored.AcknowledgeWorkerIsolation(ctx, fenced)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.ReconcileRestoredControlPlane(ctx, now.Add(2*time.Minute), proofs...); err != nil {
		t.Fatal(err)
	}
	projection, err := restored.PlanProjectionAt(ctx, version.ID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("restored prepared delivery state = %q", projection.Tickets[0].State)
	}
}

func TestRestoreRetiresSnapshottedGatewayWriteFence(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "workflow.db")
	controller, claim, now := newDeliveryFenceTestClaim(t, ctx, databasePath)
	defer controller.Close()
	gateway, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	queued, err := controller.EnqueueDelivery(ctx, DeliveryRequest{Operation: DeliveryPushCandidate, RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectRemoteAbsent: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	outboxClaim, err := controller.ClaimDeliveryOutbox(ctx, queued.IdempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ExecuteDelivery(ctx, outboxClaim.Request, outboxClaim.ClaimToken, func() time.Time { return now }, func(context.Context, DeliveryRequest) (DeliveryResult, error) {
		return DeliveryResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(directory, "workflow.backup.db")
	metadata, err := controller.CreateOnlineBackup(ctx, backupPath, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.LastDrill.Succeeded {
		t.Fatalf("snapshotted Gateway fence drill = %#v", metadata.LastDrill)
	}
	restoredPath := filepath.Join(directory, "restored.db")
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
	recovered, err := restored.DeliveryOutbox(ctx, queued.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != OutboxPending || !recovered.Uncertain || recovered.ClaimToken != "" {
		t.Fatalf("restored fenced outbox = %#v, want uncertain pending", recovered)
	}
	var fences int
	if err := restored.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_write_fences`).Scan(&fences); err != nil {
		t.Fatal(err)
	}
	if fences != 0 {
		t.Fatalf("restored Gateway write fences = %d, want 0", fences)
	}
}

func TestRestoreDryRunModelsParallelWorkerIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Title: "Plan", Labels: []string{plan.PlanLabel}},
		Children: []plan.Issue{
			{ID: 1, Number: 11, Title: "delivery", Labels: []string{plan.TicketLabel}, State: "open"},
			{ID: 2, Number: 12, Title: "agent", Labels: []string{plan.TicketLabel}, State: "open"},
		},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	first, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.AcceptCandidateForDelivery(ctx, CandidateRevision{RunID: first.RunID, LeaseToken: first.LeaseToken, CodexSessionID: "codex", CommitSHA: "accepted", StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test","outcome":"passed"}]}`), Now: now, Publication: CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveDeliveryControllerPrelaunch(ctx, delivery, now); err != nil {
		t.Fatal(err)
	}
	agent, err := db.ClaimReady(ctx, ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent-2", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveWorkerPrelaunch(ctx, agent, now); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileRestoredControlPlaneDryRun(ctx, now.Add(time.Minute)); err != nil {
		t.Fatalf("parallel Worker restore drill = %v", err)
	}
	err = db.ReconcileRestoredControlPlane(ctx, now.Add(time.Minute))
	var isolation *WorkerIsolationRequired
	if !errors.As(err, &isolation) || len(isolation.Targets) != 2 {
		t.Fatalf("parallel Worker restore isolation = %#v, %v", isolation, err)
	}
	fenced, err := db.FenceWorkerIsolation(ctx, isolation.Targets)
	if err != nil {
		t.Fatal(err)
	}
	proofs, err := db.AcknowledgeWorkerIsolation(ctx, fenced)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileRestoredControlPlane(ctx, now.Add(time.Minute), proofs...); err != nil {
		t.Fatal(err)
	}
}
