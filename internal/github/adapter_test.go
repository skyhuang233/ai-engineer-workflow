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

func TestUpdatePlanProjectionReadsTheCurrentHumanBodyAtWriteTime(t *testing.T) {
	var patched string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("ETag", `"v1"`)
			json.NewEncoder(w).Encode(issueResponse{ID: 100, Number: 10, Body: "fresh human edit"})
		case r.Method == http.MethodPatch:
			if r.Header.Get("If-Match") != `"v1"` {
				t.Errorf("If-Match = %q", r.Header.Get("If-Match"))
			}
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			patched = payload.Body
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "", server.Client())
	if err := client.UpdatePlanProjection(context.Background(), "owner/repo", 10, plan.Projection{VersionID: "pv-1", State: "Active"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patched, "fresh human edit") || !strings.Contains(patched, plan.ProjectionStart) {
		t.Fatalf("projected body = %q", patched)
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
