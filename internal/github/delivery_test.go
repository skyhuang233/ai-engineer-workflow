package github

import (
	"context"
	"encoding/json"
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

func TestDeliveryRemoteSupportsAtomicFirstPushExpectation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
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
		if strings.Contains(r.URL.Path, "/git/ref/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "head"}})
			return
		}
		pages++
		comments := make([]map[string]string, 100)
		for i := range comments {
			comments[i] = map[string]string{"body": "other"}
		}
		if r.URL.Query().Get("page") == "2" {
			comments = []map[string]string{{"body": "<!-- workflow-idempotency:key -->"}}
		}
		_ = json.NewEncoder(w).Encode(comments)
	}))
	defer server.Close()
	remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
	observation, err := remote.Observe(context.Background(), store.DeliveryRequest{Operation: store.DeliveryReplyEvidence, Repository: "owner/repo", Branch: "ticket-1", PullRequestNumber: 7, IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Applied || pages != 2 {
		t.Fatalf("observation = %#v, pages = %d", observation, pages)
	}
}

func TestDeliveryRemoteRejectsClosedOrNonMainMappedPullRequest(t *testing.T) {
	for _, pull := range []map[string]any{
		{"number": 7, "state": "closed", "head": map[string]string{"ref": "ticket-1", "sha": "candidate"}, "base": map[string]string{"ref": "main"}},
		{"number": 7, "state": "open", "head": map[string]string{"ref": "ticket-1", "sha": "candidate"}, "base": map[string]string{"ref": "release"}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(pull) }))
		remote := DeliveryRemote{Client: NewClient(server.URL, "", server.Client())}
		_, err := remote.Observe(context.Background(), store.DeliveryRequest{Operation: store.DeliveryUpsertPR, Repository: "owner/repo", Branch: "ticket-1", PullRequestNumber: 7, CommitSHA: "candidate"})
		server.Close()
		if err == nil {
			t.Fatalf("invalid mapped pull request was accepted: %#v", pull)
		}
	}
}
