package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

func TestAcquireTicketClaimReplacesExpiredWorker(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
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
	now := time.Now().UTC()
	expired, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-1", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: now.Add(-2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: expired.SessionID, AgentIdentity: "agent-1", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	replacement, prompt, err := acquireTicketClaim(ctx, db, version.ID, expired.TicketID, store.DefaultMaxWorkerAttempts, now)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "" || replacement.SessionID != expired.SessionID || replacement.Attempt != expired.Attempt+1 || replacement.LeaseGeneration != expired.LeaseGeneration+1 {
		t.Fatalf("replacement = %#v, prompt = %q", replacement, prompt)
	}
}
