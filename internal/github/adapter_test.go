package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
				{ID: 1, Number: 11, Title: "first", Labels: []labelJSON{{Name: "workflow:ticket"}}, User: userResponse{Login: "owner", Type: "User"}},
				{ID: 2, Number: 12, Title: "second", Labels: []labelJSON{{Name: "workflow:ticket"}}, User: userResponse{Login: "owner", Type: "User"}},
			})
		case "/repos/owner/repo/issues/11/dependencies/blocked_by":
			json.NewEncoder(w).Encode([]issueJSON{})
		case "/repos/owner/repo/issues/12/dependencies/blocked_by":
			json.NewEncoder(w).Encode([]issueJSON{{ID: 1, Number: 11, Title: "first", Labels: []labelJSON{{Name: "workflow:ticket"}}, User: userResponse{Login: "owner", Type: "User"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot, err := NewClient(server.URL, "token", server.Client()).WithRepositoryOwner("owner").ReadPlan(context.Background(), "owner/repo", 10)
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
			json.NewEncoder(w).Encode([]issueJSON{{ID: 1, Number: 11, Title: "partial", Labels: []labelJSON{{Name: "bug"}}, User: userResponse{Login: "owner", Type: "User"}}})
		case "/repos/owner/repo/issues/11/dependencies/blocked_by":
			json.NewEncoder(w).Encode([]issueJSON{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	snapshot, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").ReadPlan(context.Background(), "owner/repo", 10)
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

func TestReadPlanDoesNotDeriveDeliveredFromPullRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/issues/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 100, "number": 10, "labels": []map[string]string{{"name": "workflow:plan"}}, "user": map[string]string{"login": "owner", "type": "User"}})
		case "/repos/owner/repo/issues/10/sub_issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "number": 11, "state": "closed", "labels": []map[string]string{{"name": "workflow:ticket"}}, "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 2, "number": 12, "state": "open", "labels": []map[string]string{{"name": "workflow:ticket"}}, "user": map[string]string{"login": "owner", "type": "User"}}})
		case "/repos/owner/repo/issues/11/dependencies/blocked_by":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/owner/repo/issues/12/dependencies/blocked_by":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "number": 11, "state": "closed", "labels": []map[string]string{{"name": "workflow:ticket"}}, "user": map[string]string{"login": "owner", "type": "User"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	snapshot, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").ReadPlan(context.Background(), "owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Children[0].Delivered || snapshot.BlockedBy[2][0].Delivered {
		t.Fatalf("PR prose created delivery facts: %#v", snapshot)
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() accepted a closed blocker without a verified delivery fact")
	}
}

func TestActionablePullRequestFeedbackIncludesOwnerEventsOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "Please change this.", "state": "CHANGES_REQUESTED", "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 2, "body": "bot message", "state": "COMMENTED", "user": map[string]string{"login": "ci[bot]", "type": "Bot"}}, {"id": 6, "body": "", "state": "APPROVED", "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 7, "body": "foreign review", "state": "COMMENTED", "user": map[string]string{"login": "reviewer", "type": "User"}}})
		case "/repos/owner/repo/pulls/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 3, "body": "Inline concern.", "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 8, "body": "foreign inline concern", "user": map[string]string{"login": "reviewer", "type": "User"}}})
		case "/repos/owner/repo/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 4, "body": "Conversation concern.", "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 5, "body": "<!-- workflow-idempotency:x -->", "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 9, "body": "foreign conversation concern", "user": map[string]string{"login": "reviewer", "type": "User"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	events, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").ActionablePullRequestFeedback(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Source != "review" || events[1].Source != "review" || events[1].Author != "owner" || events[1].Body != "Review submitted with state: APPROVED" || events[2].Source != "inline-comment" || events[3].Source != "conversation-comment" {
		t.Fatalf("events = %#v", events)
	}
}

func TestPullRequestChecksReadsCurrentCandidateChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/commits/candidate/check-runs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{{"id": 42, "name": "workflow-contract", "status": "completed", "conclusion": "success", "head_sha": "candidate"}}})
	}))
	defer server.Close()
	checks, err := NewClient(server.URL, "", server.Client()).PullRequestChecks(context.Background(), "owner/repo", "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].CheckRunID != 42 || checks[0].Conclusion != "success" || checks[0].HeadSHA != "candidate" {
		t.Fatalf("checks = %#v", checks)
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
	client := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner")
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
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 42, "body": planProjectionIdentity, "user": map[string]string{"login": "owner", "type": "User"}}})
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
	if err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").UpdatePlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"}); err != nil {
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
		{{"id": 1, "body": marker, "user": map[string]string{"login": "owner", "type": "User"}}},
		{{"id": 1, "body": planProjectionIdentity + marker, "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 2, "body": planProjectionIdentity, "user": map[string]string{"login": "owner", "type": "User"}}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(comments) }))
		_, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").HasPlanProjection(context.Background(), "owner/repo", 10, projection)
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
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "<!-- workflow-projection:obsolete -->", "user": map[string]string{"login": "owner", "type": "User"}}})
	}))
	defer server.Close()

	err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").UpdatePlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"})
	if err == nil || !strings.Contains(err.Error(), "legacy workflow projection comment") {
		t.Fatalf("UpdatePlanProjection error = %v", err)
	}
}

func TestHasPlanProjectionRejectsLegacyMarkerRegardlessOfDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "<!-- workflow-projection:obsolete -->", "user": map[string]string{"login": "owner", "type": "User"}}})
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").HasPlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"})
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

func TestProjectionObservationsIgnoreNonOwnerArtifacts(t *testing.T) {
	projection := plan.Projection{VersionID: "pv-1", State: "Active"}
	marker := planProjectionMarker(projection)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": planProjectionIdentity + marker, "user": map[string]string{"login": "outsider", "type": "User"}}})
	}))
	defer server.Close()
	applied, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").HasPlanProjection(context.Background(), "owner/repo", 10, projection)
	if err != nil || applied {
		t.Fatalf("non-owner projection observation = %t, %v", applied, err)
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

func TestRequirePublicRepositoryRejectsPrivateRepositories(t *testing.T) {
	private := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"private": private})
	}))
	defer server.Close()
	client := NewClient(server.URL, "", server.Client())
	if err := client.RequirePublicRepository(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("public repository: %v", err)
	}
	private = true
	if err := client.RequirePublicRepository(context.Background(), "owner/repo"); err == nil || !strings.Contains(err.Error(), "must be public") {
		t.Fatalf("private repository error = %v", err)
	}
}

func TestWorkflowInboxAnswersExtractsKnownIdAddressedReplies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/issues":
			if r.Method != http.MethodGet || r.URL.Query().Get("labels") != workflowInboxLabel {
				t.Fatalf("request = %s %s", r.Method, r.URL.String())
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 10, "number": 10, "title": "Workflow Inbox", "body": "", "labels": []map[string]string{{"name": workflowInboxLabel}}, "user": map[string]string{"login": "owner", "type": "User"}}})
		case "/repos/owner/repo/issues/10/comments":
			if r.Method != http.MethodGet {
				t.Fatalf("request = %s %s", r.Method, r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "workflow-answer:needs-attention-pv-1-1-g1: retry after restoring access\nworkflow-answer:unknown: ignored", "user": map[string]string{"login": "owner", "type": "User"}}, {"id": 2, "body": "workflow-answer:needs-attention-pv-1-1-g1: cancel-plan", "user": map[string]string{"login": "reviewer", "type": "User"}}})
		default:
			t.Fatalf("request = %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	answers, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").WorkflowInboxAnswers(context.Background(), "owner/repo", []string{"needs-attention-pv-1-1-g1"})
	if err != nil {
		t.Fatal(err)
	}
	if answers["needs-attention-pv-1-1-g1"] != "retry after restoring access" || len(answers) != 1 {
		t.Fatalf("answers = %#v", answers)
	}
}

func TestRateLimitRetryAtUsesGitHubReset(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-RateLimit-Remaining", "0")
	headers.Set("X-RateLimit-Reset", "1785715260")
	response := &http.Response{StatusCode: http.StatusForbidden, Header: headers}
	got := rateLimitRetryAt(response, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))
	if !got.Equal(time.Unix(1785715260, 0).UTC()) {
		t.Fatalf("retry at = %s", got)
	}
}

func TestAPIErrorDistinguishesRateLimitedForbiddenFromCredentialRejection(t *testing.T) {
	retryAt := time.Date(2026, 8, 4, 0, 7, 0, 0, time.UTC)
	if (&apiError{StatusCode: http.StatusForbidden, RetryAt: retryAt}).AuthenticationFailure() {
		t.Fatal("rate-limited forbidden response was classified as credential rejection")
	}
	if !(&apiError{StatusCode: http.StatusForbidden}).AuthenticationFailure() {
		t.Fatal("unqualified forbidden response was not classified as credential rejection")
	}
}

type issueJSON struct {
	ID     int64        `json:"id"`
	Number int64        `json:"number"`
	Title  string       `json:"title"`
	Labels []labelJSON  `json:"labels"`
	User   userResponse `json:"user"`
}

type labelJSON struct {
	Name string `json:"name"`
}

func writeIssue(w http.ResponseWriter, id, number int64, title string, labels []string) {
	converted := make([]labelJSON, len(labels))
	for i, label := range labels {
		converted[i] = labelJSON{Name: label}
	}
	json.NewEncoder(w).Encode(issueJSON{ID: id, Number: number, Title: title, Labels: converted, User: userResponse{Login: "owner", Type: "User"}})
}

func TestReadPlanRejectsNonOwnerPlanInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/repos/owner/repo/issues/10" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 100, "number": 10, "labels": []map[string]string{{"name": "workflow:plan"}}, "user": map[string]string{"login": "outsider", "type": "User"}})
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "", server.Client()).WithRepositoryOwner("owner").ReadPlan(context.Background(), "owner/repo", 10)
	if err == nil || !strings.Contains(err.Error(), "not the configured repository owner") {
		t.Fatalf("non-owner plan error = %v", err)
	}
}
