package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type containerIsolatorFunc func(context.Context, string) error

func (f containerIsolatorFunc) IsolateContainer(ctx context.Context, runID string) error {
	return f(ctx, runID)
}

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
	now := time.Date(2099, 8, 9, 0, 0, 0, 0, time.UTC)
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
	if err := db.ReserveDeliveryControllerPrelaunch(ctx, deliveryClaim, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveDeliveryControllerLaunch(ctx, deliveryClaim, store.WorkerAudit{RunID: deliveryClaim.RunID, LeaseGeneration: deliveryClaim.LeaseGeneration, ImageDigest: "sha256:delivery", ToolVersions: map[string]string{"codex": "1", "github-cli": "1", "go": "1", "no-mistakes": "1"}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case "/repos/owner/repo/git/ref/heads/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "merged-on-main"}})
		case "/repos/owner/repo/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "closed", "merged_at": "2026-08-09T00:00:00Z", "merge_commit_sha": "merged-on-main",
				"merged_by": map[string]string{"login": "owner", "type": "User"}, "base": map[string]string{"ref": "main", "sha": "merged-on-main"}, "head": map[string]string{"sha": "accepted-candidate"},
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
	var isolated []string
	var downstream store.TicketClaim
	poller := Poller{
		Store: db, Client: NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner"), Now: func() time.Time { return now.Add(time.Minute) },
		ContainerIsolator: containerIsolatorFunc(func(_ context.Context, runID string) error {
			target, err := db.DeliveryContainerIsolationTarget(ctx, version.ID, 1)
			if err != nil || target.RunID != runID {
				return fmt.Errorf("delivery isolation target = %#v, %v", target, err)
			}
			isolated = append(isolated, runID)
			return nil
		}),
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
	if len(isolated) != 1 || isolated[0] != deliveryClaim.RunID {
		t.Fatalf("isolated delivery runs = %#v, want [%q]", isolated, deliveryClaim.RunID)
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
	t.Logf("owner-merged PR #7 delivered Ticket 1; merge_commit=%s; dependent Ticket %d became dispatchable", stored.MergeCommit, downstream.TicketID)
}

func TestMergedParallelCandidateInvalidatesOtherMergeReadyCandidate(t *testing.T) {
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
	claims := make(map[int64]store.TicketClaim)
	for _, ticketID := range []int64{1, 2} {
		claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: ticketID, Owner: "agent", MaxParallelRuns: 2, LeaseTTL: time.Hour, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		claims[ticketID] = claim
		if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent", WorkspacePath: "workspace", CodexStatePath: "codex", Branch: "ticket-" + strconv.FormatInt(ticketID, 10)}); err != nil {
			t.Fatal(err)
		}
		candidate := "candidate-" + strconv.FormatInt(ticketID, 10)
		deliveryClaim, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{
			RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex", CommitSHA: candidate,
			StructuredOutput: []byte(`{"summary":"candidate","checks":[{"command":"go test ./...","outcome":"passed"}]}`),
			Now:              now, Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-" + strconv.FormatInt(ticketID, 10), ExpectRemoteAbsent: true, Title: "ticket"},
		}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecordDeliveryMapping(ctx, store.DeliveryRequest{Operation: store.DeliveryUpsertPR, RunID: deliveryClaim.RunID, LeaseToken: deliveryClaim.LeaseToken, LeaseGeneration: deliveryClaim.LeaseGeneration, Repository: snapshot.Repository, Branch: "ticket-" + strconv.FormatInt(ticketID, 10), CommitSHA: candidate, ExpectedRemoteHead: candidate, Title: "ticket"}, 10+ticketID, "PR_node", candidate, now); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteDeliveryController(ctx, deliveryClaim, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	secondDelivery, err := db.CandidateDelivery(ctx, version.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated, err := db.ObserveMergeReady(ctx, secondDelivery, store.MergeReadyObservation{DefaultBranch: "main", DefaultBranchHead: "main-1", BaseBranch: "main", BaseCommit: "main-1", CandidateHead: "candidate-2", CandidateIncludesDefault: true, ChecksPassed: true, HumanReviewed: true}, now.Add(2*time.Second)); err != nil || invalidated {
		t.Fatalf("initial second candidate observation invalidated=%t err=%v", invalidated, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case "/repos/owner/repo/git/ref/heads/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "main-2"}})
		case "/repos/owner/repo/pulls/11":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed", "merged_at": "2026-08-09T00:00:00Z", "merge_commit_sha": "main-2", "merged_by": map[string]string{"login": "owner", "type": "User"}, "base": map[string]string{"ref": "main", "sha": "main-2"}, "head": map[string]string{"sha": "candidate-1"}})
		case "/repos/owner/repo/pulls/12":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "open", "base": map[string]string{"ref": "main", "sha": "main-2"}, "head": map[string]string{"sha": "candidate-2"}})
		case "/repos/owner/repo/compare/candidate-1...main":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ahead"})
		case "/repos/owner/repo/compare/main-2...candidate-2":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "diverged"})
		case "/repos/owner/repo/commits/candidate-1/check-runs", "/repos/owner/repo/commits/candidate-2/check-runs":
			candidate := "candidate-1"
			if r.URL.Path == "/repos/owner/repo/commits/candidate-2/check-runs" {
				candidate = "candidate-2"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "quality", "status": "completed", "conclusion": "success", "head_sha": candidate}}})
		case "/repos/owner/repo/pulls/12/reviews":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"state": "APPROVED", "commit_id": "candidate-2", "user": map[string]string{"login": "owner", "type": "User"}}})
		case "/repos/owner/repo/pulls/12/comments", "/repos/owner/repo/issues/12/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var revision store.TicketClaim
	poller := Poller{
		Store: db, Client: NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner"), Now: func() time.Time { return now.Add(3 * time.Second) }, MaxParallelRuns: 2,
		LaunchReview: func(_ context.Context, claim store.TicketClaim, prompt string) error {
			revision = claim
			if !strings.Contains(prompt, "Default branch main advanced from main-1 to main-2") {
				t.Fatalf("revalidation prompt = %q", prompt)
			}
			return nil
		},
	}
	result, err := poller.Poll(ctx, snapshot.Repository)
	if err != nil || result.Delivered != 1 || result.Feedback != 1 {
		t.Fatalf("poll result = %#v, err=%v", result, err)
	}
	if revision.SessionID != claims[2].SessionID || revision.Attempt != claims[2].Attempt+1 {
		t.Fatalf("revision = %#v, want a new Revision Round for session %q", revision, claims[2].SessionID)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delivered first ticket retained a claim: %v", err)
	}

	redelivery, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{
		RunID: revision.RunID, LeaseToken: revision.LeaseToken, CodexSessionID: "codex", CommitSHA: "candidate-2-rebased",
		StructuredOutput: []byte(`{"summary":"rebased","checks":[{"command":"go test ./...","outcome":"passed"}]}`),
		Now:              now.Add(4 * time.Second), Publication: store.CandidatePublication{Repository: snapshot.Repository, Branch: "ticket-2", ExpectedRemoteHead: "candidate-2", Title: "second"},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeliveryController(ctx, redelivery, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, err := db.CandidateDelivery(ctx, version.ID, 2)
	if err != nil || updated.PullRequestNumber != 12 || updated.CandidateCommit != "candidate-2-rebased" || updated.Branch != "ticket-2" {
		t.Fatalf("revalidated delivery = %#v, err=%v", updated, err)
	}
}
