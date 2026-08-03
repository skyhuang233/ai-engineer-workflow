package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/plan"
)

func TestReadPlanUsesNativeSubIssuesAndBlockedByEndpoints(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/issues/10":
			writeIssue(w, 100, 10, "Plan", []string{"workflow:plan"})
		case "/repos/owner/repo/issues/10/sub_issues":
			json.NewEncoder(w).Encode([]issueJSON{
				{ID: 1, Number: 11, Title: "first", Labels: []labelJSON{{Name: "workflow:ticket"}}},
				{ID: 2, Number: 12, Title: "second", Labels: []labelJSON{{Name: "workflow:ticket"}}},
			})
		case "/repos/owner/repo/issues/11/dependencies/blocked_by":
			json.NewEncoder(w).Encode([]issueJSON{})
		case "/repos/owner/repo/issues/12/dependencies/blocked_by":
			json.NewEncoder(w).Encode([]issueJSON{{ID: 1, Number: 11, Title: "first", Labels: []labelJSON{{Name: "workflow:ticket"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot, err := NewClient(server.URL, "token", server.Client()).ReadPlan(context.Background(), "owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Children) != 2 || len(snapshot.BlockedBy[2]) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if len(requests) != 4 || requests[1] != "GET /repos/owner/repo/issues/10/sub_issues" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestReadPlanRetainsUntypedChildForIncompletePublication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/issues/10":
			writeIssue(w, 100, 10, "Plan", []string{"workflow:plan"})
		case "/repos/owner/repo/issues/10/sub_issues":
			json.NewEncoder(w).Encode([]issueJSON{{ID: 1, Number: 11, Title: "partial", Labels: []labelJSON{{Name: "bug"}}}})
		case "/repos/owner/repo/issues/11/dependencies/blocked_by":
			json.NewEncoder(w).Encode([]issueJSON{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	snapshot, err := NewClient(server.URL, "", server.Client()).ReadPlan(context.Background(), "owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Children) != 1 || snapshot.Children[0].IsTicket() {
		t.Fatalf("untyped child was dropped or typed: %#v", snapshot.Children)
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() accepted incomplete publication")
	}
}

func TestActionablePullRequestFeedbackIncludesHumanEventsOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "Please change this.", "state": "CHANGES_REQUESTED", "user": map[string]string{"login": "reviewer", "type": "User"}}, {"id": 2, "body": "bot message", "state": "COMMENTED", "user": map[string]string{"login": "ci[bot]", "type": "Bot"}}})
		case "/repos/owner/repo/pulls/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 3, "body": "Inline concern.", "user": map[string]string{"login": "reviewer", "type": "User"}}})
		case "/repos/owner/repo/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 4, "body": "Conversation concern.", "user": map[string]string{"login": "reviewer", "type": "User"}}, {"id": 5, "body": "<!-- workflow-idempotency:x -->", "user": map[string]string{"login": "workflow", "type": "User"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	events, err := NewClient(server.URL, "", server.Client()).ActionablePullRequestFeedback(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Source != "review" || events[1].Source != "inline-comment" || events[2].Source != "conversation-comment" {
		t.Fatalf("events = %#v", events)
	}
}

func TestUpdateIssueBodyPreservesPatchPayloadAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/owner/repo/issues/10" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
			t.Fatalf("API version = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "human spec") {
			t.Fatalf("body = %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := NewClient(server.URL, "", server.Client()).UpdateIssueBody(context.Background(), "owner/repo", 10, "human spec"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatePlanProjectionCreatesOneStatusComment(t *testing.T) {
	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/10/comments" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/repos/owner/repo/issues/10/comments" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		comment = payload.Body
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	client := NewClient(server.URL, "", server.Client())
	if err := client.UpdatePlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(comment, plan.ProjectionStart) || !strings.Contains(comment, planProjectionIdentity) || !strings.Contains(comment, "workflow-projection:") {
		t.Fatalf("projected comment = %q", comment)
	}
}

func TestUpdatePlanProjectionUpdatesTheExistingStatusComment(t *testing.T) {
	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/10/comments" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 42, "body": planProjectionIdentity}})
			return
		}
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/owner/repo/issues/comments/42" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		comment = payload.Body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := NewClient(server.URL, "", server.Client()).UpdatePlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(comment, "state: `Active`") {
		t.Fatalf("projected comment = %q", comment)
	}
}

func TestHasPlanProjectionRejectsLegacyAndDuplicateStatusComments(t *testing.T) {
	projection := plan.Projection{VersionID: "pv-1", State: "Active"}
	marker := planProjectionMarker(projection)
	for _, comments := range [][]map[string]any{
		{{"id": 1, "body": marker}},
		{{"id": 1, "body": planProjectionIdentity + marker}, {"id": 2, "body": planProjectionIdentity}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(comments) }))
		_, err := NewClient(server.URL, "", server.Client()).HasPlanProjection(context.Background(), "owner/repo", 10, projection)
		server.Close()
		if err == nil {
			t.Fatalf("invalid status comments were accepted: %#v", comments)
		}
	}
}

func TestUpdatePlanProjectionRejectsLegacyMarkerRegardlessOfDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("legacy projection comment allowed mutation: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "<!-- workflow-projection:obsolete -->"}})
	}))
	defer server.Close()

	err := NewClient(server.URL, "", server.Client()).UpdatePlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"})
	if err == nil || !strings.Contains(err.Error(), "legacy workflow projection comment") {
		t.Fatalf("UpdatePlanProjection error = %v", err)
	}
}

func TestHasPlanProjectionRejectsLegacyMarkerRegardlessOfDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "<!-- workflow-projection:obsolete -->"}})
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "", server.Client()).HasPlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"})
	if err == nil || !strings.Contains(err.Error(), "legacy workflow projection comment") {
		t.Fatalf("HasPlanProjection error = %v", err)
	}
}

func TestDeliveredLabelIsProjectionOnly(t *testing.T) {
	issue := issueResponse{ID: 1, Number: 11, State: "closed", Labels: []labelResponse{{Name: "workflow:ticket"}, {Name: "workflow:delivered"}}}.issue()
	if issue.IsDelivered() {
		t.Fatal("workflow:delivered label became authoritative delivery state")
	}
}

func TestAddIssueLabelUsesAddLabelsEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/owner/repo/issues/10/labels" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "workflow:active") {
			t.Fatalf("body = %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := NewClient(server.URL, "", server.Client()).AddIssueLabel(context.Background(), "owner/repo", 10, "workflow:active"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRepository(t *testing.T) {
	if err := ValidateRepository("owner/repo"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRepository("owner/repo/extra"); err == nil {
		t.Fatal("accepted repository with too many path components")
	}
}

type issueJSON struct {
	ID     int64       `json:"id"`
	Number int64       `json:"number"`
	Title  string      `json:"title"`
	Labels []labelJSON `json:"labels"`
}

type labelJSON struct {
	Name string `json:"name"`
}

func writeIssue(w http.ResponseWriter, id, number int64, title string, labels []string) {
	converted := make([]labelJSON, len(labels))
	for i, label := range labels {
		converted[i] = labelJSON{Name: label}
	}
	json.NewEncoder(w).Encode(issueJSON{ID: id, Number: number, Title: title, Labels: converted})
}
