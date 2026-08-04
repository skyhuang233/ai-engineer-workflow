package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type recordingPusher struct {
	expected string
	absent   bool
}

func (p *recordingPusher) Push(_ context.Context, _, _, _, expected string, absent bool) error {
	p.expected = expected
	p.absent = absent
	return nil
}

func TestDeliveryRemoteRefreshesCredentialAtDispatchBoundary(t *testing.T) {
	token := "github_pat_before"
	remote := DeliveryRemote{
		Client: NewClient("https://api.github.com", "github_pat_stale", nil),
		CredentialSource: func(context.Context) (string, error) {
			return token, nil
		},
	}
	if err := remote.CredentialAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remote.client().Token; got != "github_pat_before" {
		t.Fatalf("initial credential = %q", got)
	}
	token = "github_pat_after"
	if err := remote.CredentialAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remote.client().Token; got != "github_pat_after" {
		t.Fatalf("refreshed credential = %q", got)
	}
}

func TestDeliveryRemoteSupportsAtomicFirstPushExpectation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
			_, _ = w.Write([]byte(`{"private":false}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	pusher := &recordingPusher{}
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client()), Pusher: pusher}
	request := store.DeliveryRequest{Operation: store.DeliveryPushCandidate, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "candidate", ExpectRemoteAbsent: true}
	observation, err := remote.Observe(context.Background(), request)
	if err != nil || observation.RemoteExists {
		t.Fatalf("absent observation = %#v, err = %v", observation, err)
	}
	if _, err := remote.Apply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !pusher.absent || pusher.expected != "" {
		t.Fatalf("push expectation = expected %q absent %t", pusher.expected, pusher.absent)
	}
}

func TestDeliveryRemoteWritesPlanProjectionAsStatusComment(t *testing.T) {
	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
			_, _ = w.Write([]byte(`{"private":false}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/10/comments" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/repos/owner/repo/issues/10/comments" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode comment payload: %v", err)
		}
		comment = payload.Body
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	projection := plan.Projection{VersionID: "pv-1", State: "Active"}
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner")}
	if _, err := remote.Apply(context.Background(), store.DeliveryRequest{Operation: store.DeliveryProjectPlan, Repository: "owner/repo", RootNumber: 10, PlanProjection: &projection}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(comment, plan.ProjectionStart) || !strings.Contains(comment, "workflow-projection:") {
		t.Fatalf("projected comment = %s", comment)
	}
}

func TestDeliveryRemotePaginatesEvidenceReconciliation(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
			_, _ = w.Write([]byte(`{"private":false}`))
			return
		}
		if strings.Contains(r.URL.Path, "/git/ref/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "head"}})
			return
		}
		pages++
		comments := make([]map[string]any, 100)
		for i := range comments {
			comments[i] = map[string]any{"body": "other", "user": map[string]string{"login": "owner", "type": "User"}}
		}
		if r.URL.Query().Get("page") == "2" {
			comments = []map[string]any{{"body": "<!-- workflow-idempotency:key -->", "user": map[string]string{"login": "owner", "type": "User"}}}
		}
		_ = json.NewEncoder(w).Encode(comments)
	}))
	defer server.Close()
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner")}
	observation, err := remote.Observe(context.Background(), store.DeliveryRequest{Operation: store.DeliveryReplyEvidence, Repository: "owner/repo", Branch: "ticket-1", PullRequestNumber: 7, IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Applied || pages != 2 {
		t.Fatalf("observation = %#v, pages = %d", observation, pages)
	}
}

func TestDeliveryRemoteIgnoresNonOwnerEvidenceMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
			_, _ = w.Write([]byte(`{"private":false}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/git/ref/heads/ticket-1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "head"}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"body": "<!-- workflow-idempotency:key -->", "user": map[string]string{"login": "reviewer", "type": "User"}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner")}
	observation, err := remote.Observe(context.Background(), store.DeliveryRequest{Operation: store.DeliveryReplyEvidence, Repository: "owner/repo", Branch: "ticket-1", PullRequestNumber: 7, IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Applied {
		t.Fatalf("non-owner evidence marker was accepted: %#v", observation)
	}
}

func TestDeliveryRemoteRejectsClosedOrNonMainMappedPullRequest(t *testing.T) {
	for _, pull := range []map[string]any{
		{"number": 7, "state": "closed", "head": map[string]string{"ref": "ticket-1", "sha": "candidate"}, "base": map[string]string{"ref": "main"}},
		{"number": 7, "state": "open", "head": map[string]string{"ref": "ticket-1", "sha": "candidate"}, "base": map[string]string{"ref": "release"}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/owner/repo" {
				_, _ = w.Write([]byte(`{"private":false}`))
				return
			}
			_ = json.NewEncoder(w).Encode(pull)
		}))
		remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
		_, err := remote.Observe(context.Background(), store.DeliveryRequest{Operation: store.DeliveryUpsertPR, Repository: "owner/repo", Branch: "ticket-1", PullRequestNumber: 7, CommitSHA: "candidate"})
		server.Close()
		if err == nil {
			t.Fatalf("invalid mapped pull request was accepted: %#v", pull)
		}
	}
}

func TestDeliveryRemoteRejectsPrivateRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo" {
			_, _ = w.Write([]byte(`{"private":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
	_, err := remote.Observe(context.Background(), store.DeliveryRequest{Operation: store.DeliveryPushCandidate, Repository: "owner/repo"})
	if !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("private repository error = %v", err)
	}
}

func TestDeliveryRemoteRejectsPullRequestWhoseHeadChangedDuringApply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
			_, _ = w.Write([]byte(`{"private":false}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open", "head": map[string]string{"ref": "ticket-1", "sha": "replacement"}, "base": map[string]string{"ref": "main"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
	_, err := remote.Apply(context.Background(), store.DeliveryRequest{Operation: store.DeliveryUpsertPR, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "accepted", Title: "ticket"})
	if err == nil || !errors.Is(err, store.ErrDeliveryRejected) {
		t.Fatalf("head-change error = %v", err)
	}
}

func TestDeliveryRemoteRejectsInvalidPullRequestReturnedByApply(t *testing.T) {
	for _, pull := range []map[string]any{
		{"number": 7, "state": "closed", "head": map[string]string{"ref": "ticket-1", "sha": "accepted"}, "base": map[string]string{"ref": "main"}},
		{"number": 7, "state": "open", "head": map[string]string{"ref": "other", "sha": "accepted"}, "base": map[string]string{"ref": "main"}},
		{"number": 7, "state": "open", "head": map[string]string{"ref": "ticket-1", "sha": "accepted"}, "base": map[string]string{"ref": "release"}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
				_, _ = w.Write([]byte(`{"private":false}`))
				return
			}
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]any{})
			case http.MethodPost:
				_ = json.NewEncoder(w).Encode(pull)
			default:
				http.NotFound(w, r)
			}
		}))
		remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
		_, err := remote.Apply(context.Background(), store.DeliveryRequest{Operation: store.DeliveryUpsertPR, Repository: "owner/repo", Branch: "ticket-1", CommitSHA: "accepted", ExpectedRemoteHead: "accepted", Title: "ticket"})
		server.Close()
		if !errors.Is(err, store.ErrDeliveryRejected) {
			t.Fatalf("invalid response error = %v", err)
		}
	}
}
