package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

func TestPullRequestReachedMainRequiresMappedCandidateReachability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "closed", "merged_at": "2026-07-31T00:00:00Z", "merge_commit_sha": "merged-other-revision",
				"merged_by": map[string]string{"login": "owner", "type": "User"},
				"base":      map[string]string{"ref": "main"},
			})
		case "/repos/owner/repo/compare/accepted-candidate...main":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ahead"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reached, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").pullRequestReachedMain(context.Background(), "owner/repo", 7, "accepted-candidate")
	if err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("mapped candidate reachable from main was not delivered")
	}
}

func TestPullRequestReachedMainRejectsNonOwnerAndBotMergers(t *testing.T) {
	for _, mergedBy := range []map[string]string{{"login": "reviewer", "type": "User"}, {"login": "owner[bot]", "type": "Bot"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/owner/repo/pulls/7" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "closed", "merged_at": "2026-07-31T00:00:00Z", "merge_commit_sha": "merged", "merged_by": mergedBy,
				"base": map[string]string{"ref": "main"},
			})
		}))
		reached, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").pullRequestReachedMain(context.Background(), "owner/repo", 7, "accepted-candidate")
		server.Close()
		if err != nil || reached {
			t.Fatalf("merged_by=%#v reached=%t err=%v", mergedBy, reached, err)
		}
	}
}

func TestClosedUnmergedPullRequestFreezesInsteadOfDelivering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/7" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed", "merged_at": nil, "merge_commit_sha": nil, "base": map[string]string{"ref": "main"}})
	}))
	defer server.Close()
	delivery, err := NewClient(server.URL, "", server.Client()).pullRequestDelivery(context.Background(), "owner/repo", 7, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != pullRequestClosedUnmerged {
		t.Fatalf("state = %d, want closed-unmerged", delivery.State)
	}
}

func TestReconcileTicketPersistsMergeRevisionAndUnlocksDependentFrontier(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 100, Labels: []string{plan.PlanLabel}},
		Children: []plan.Issue{
			{ID: 1, Number: 1, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"},
			{ID: 2, Number: 2, Title: "second", Labels: []string{plan.TicketLabel}, State: "open"},
		},
		BlockedBy: map[int64][]plan.Issue{2: {{ID: 1, Number: 1, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}},
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
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	deliveryClaim, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{
		RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: "accepted-candidate",
		StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test ./...","outcome":"passed"}]}`),
		Now:              now,
		Publication:      store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "first"},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeliveryMapping(ctx, store.DeliveryRequest{
		Operation: store.DeliveryUpsertPR, RunID: deliveryClaim.RunID, LeaseToken: deliveryClaim.LeaseToken, LeaseGeneration: deliveryClaim.LeaseGeneration,
		Repository: snapshot.Repository, Branch: "ticket-1", CommitSHA: "accepted-candidate", ExpectedRemoteHead: "accepted-candidate", Title: "first",
	}, 7, "PR_node", "accepted-candidate", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "closed", "merged_at": "2026-08-09T00:00:00Z", "merge_commit_sha": "merged-on-main",
				"merged_by": map[string]string{"login": "owner", "type": "User"}, "base": map[string]string{"ref": "main"},
			})
		case "/repos/owner/repo/compare/accepted-candidate...main":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ahead"})
		case "/repos/owner/repo/commits/accepted-candidate/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	launched := 0
	var downstream store.TicketClaim
	poller := Poller{
		Store: db, Client: NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner"), Now: func() time.Time { return now.Add(time.Minute) },
		AfterDelivered: func(ctx context.Context) error {
			launched++
			var err error
			downstream, err = db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 2, Owner: "agent", MaxParallelRuns: 1, LeaseTTL: time.Hour, Now: now.Add(time.Minute)})
			return err
		},
	}
	result, err := poller.Poll(ctx, snapshot.Repository)
	if err != nil || result.Delivered != 1 || launched != 1 || downstream.TicketID != 2 {
		t.Fatalf("first poll result=%#v launched=%d claim=%#v err=%v", result, launched, downstream, err)
	}
	stored, err := db.CandidateDelivery(ctx, version.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MergeCommit != "merged-on-main" {
		t.Fatalf("merge commit = %q, want merged-on-main", stored.MergeCommit)
	}
	duplicate, err := poller.Poll(ctx, snapshot.Repository)
	if err != nil || duplicate.Delivered != 0 || launched != 1 {
		t.Fatalf("duplicate poll result=%#v launched=%d err=%v", duplicate, launched, err)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delivered current claim error = %v, want ErrNotFound", err)
	}
	if current, err := db.CurrentClaim(ctx, version.ID, 2); err != nil || current.RunID != downstream.RunID {
		t.Fatalf("downstream dispatch = %#v, %v; want %#v", current, err, downstream)
	}
}
