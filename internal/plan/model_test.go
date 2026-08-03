package plan

import (
	"errors"
	"strings"
	"testing"
)

func issue(id, number int64, labels ...string) Issue {
	return Issue{ID: id, Number: number, Title: "Ticket", Labels: labels, State: "open"}
}

func validSnapshot() Snapshot {
	return Snapshot{
		Repository: "owner/repo",
		Root:       issue(100, 10, PlanLabel),
		Children: []Issue{
			issue(1, 11, TicketLabel),
			issue(2, 12, TicketLabel),
		},
		BlockedBy: map[int64][]Issue{2: {issue(1, 11, TicketLabel)}},
	}
}

func TestSnapshotValidateAcceptsTypedAcyclicGraph(t *testing.T) {
	if err := validSnapshot().Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestSnapshotValidateRejectsPlanRootAsTicket(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.Root.Labels = append(snapshot.Root.Labels, TicketLabel)
	if err := snapshot.Validate(); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Validate() error = %v, want ErrInvalidPlan", err)
	}
}

func TestSnapshotValidateRejectsUntypedPartialPublication(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.Children = append(snapshot.Children, issue(3, 13, "bug"))
	if err := snapshot.Validate(); !errors.Is(err, ErrIncompletePublish) {
		t.Fatalf("Validate() error = %v, want ErrIncompletePublish", err)
	}
}

func TestSnapshotValidateRejectsUnknownBlocker(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.BlockedBy[2] = append(snapshot.BlockedBy[2], issue(99, 99, TicketLabel))
	if err := snapshot.Validate(); !errors.Is(err, ErrUnknownBlocker) {
		t.Fatalf("Validate() error = %v, want ErrUnknownBlocker", err)
	}
}

func TestSnapshotValidateRejectsCycleAndDuplicateEdge(t *testing.T) {
	cycle := validSnapshot()
	cycle.BlockedBy[1] = []Issue{issue(2, 12, TicketLabel)}
	if err := cycle.Validate(); !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle Validate() error = %v, want ErrCycle", err)
	}

	duplicate := validSnapshot()
	duplicate.BlockedBy[2] = append(duplicate.BlockedBy[2], issue(1, 11, TicketLabel))
	if err := duplicate.Validate(); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("duplicate Validate() error = %v, want ErrDuplicateNode", err)
	}
}

func TestSnapshotValidateRejectsClosedUndeliveredBlocker(t *testing.T) {
	snapshot := validSnapshot()
	closed := issue(1, 11, TicketLabel)
	closed.State = "closed"
	snapshot.Children[0] = closed
	snapshot.BlockedBy[2] = []Issue{closed}
	if err := snapshot.Validate(); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Validate() error = %v, want ErrInvalidPlan", err)
	}
}

func TestFingerprintIgnoresExistingProjection(t *testing.T) {
	first := validSnapshot()
	first.Root.Body = "human spec"
	second := validSnapshot()
	second.Root.Body = "human spec\n\n" + ProjectionStart + "\nold\n" + ProjectionEnd
	a, err := first.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprints differ after projection update: %s != %s", a, b)
	}
}

func TestFingerprintIgnoresActivationLabel(t *testing.T) {
	first := validSnapshot()
	second := validSnapshot()
	second.Root.Labels = append(second.Root.Labels, ActiveLabel)
	a, err := first.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprints differ after activation label: %s != %s", a, b)
	}
}

func TestFingerprintIgnoresMutableDeliveryFacts(t *testing.T) {
	first := validSnapshot()
	second := validSnapshot()
	second.Children[0].State = "closed"
	second.Children[0].Delivered = true
	second.Children[0].Labels = append(second.Children[0].Labels, "workflow:delivered")
	a, err := first.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprints differ after delivery facts: %s != %s", a, b)
	}
}

func TestDeliveredLabelDoesNotAuthorizeDelivery(t *testing.T) {
	issue := Issue{Labels: []string{TicketLabel, "workflow:delivered"}}
	if issue.IsDelivered() {
		t.Fatal("delivery projection label was treated as an authoritative fact")
	}
	issue.Delivered = true
	if !issue.IsDelivered() {
		t.Fatal("verified delivery fact was ignored")
	}
}

func TestRenderProjectionPreservesHumanSpec(t *testing.T) {
	body := "# Approved spec\n\nKeep this text.\n\n" + ProjectionStart + "\nold\n" + ProjectionEnd + "\n\nHuman notes."
	updated, err := RenderProjection(body, Projection{VersionID: "pv-1", State: "Active", Tickets: []ProjectionTicket{{Number: 11, Title: "A | B", Blockers: []int64{12}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(updated, "# Approved spec\n\nKeep this text.") || !strings.HasSuffix(updated, "Human notes.") {
		t.Fatalf("human spec was not preserved: %q", updated)
	}
	if !strings.Contains(updated, "| #11 A \\| B | #12 |") {
		t.Fatalf("ticket projection missing: %q", updated)
	}
}

func TestRenderProjectionRejectsHalfWrittenMarker(t *testing.T) {
	_, err := RenderProjection("spec\n"+ProjectionStart, Projection{VersionID: "pv-1", State: "Active"})
	if !errors.Is(err, ErrMalformedStatus) {
		t.Fatalf("RenderProjection() error = %v, want ErrMalformedStatus", err)
	}
}

func TestRenderProjectionRejectsMultipleMarkerBlocks(t *testing.T) {
	body := ProjectionStart + "one" + ProjectionEnd + "\n" + ProjectionStart + "two" + ProjectionEnd
	if _, err := RenderProjection(body, Projection{VersionID: "pv-1", State: "Active"}); !errors.Is(err, ErrMalformedStatus) {
		t.Fatalf("RenderProjection() error = %v, want ErrMalformedStatus", err)
	}
}
