package plan

import "testing"

func TestReadyFrontierIsStableAndHonorsDeliveredBlockersAndCapacity(t *testing.T) {
	snapshot := FrontierSnapshot{
		VersionID: "pv-1",
		PlanState: StateActive,
		Tickets: []FrontierTicket{
			{IssueID: 3, Number: 30, Title: "downstream", Delivered: false},
			{IssueID: 1, Number: 20, Title: "second", Delivered: false},
			{IssueID: 2, Number: 10, Title: "first", Delivered: true},
			{IssueID: 4, Number: 40, Title: "owned", Owner: "agent-1"},
		},
		Dependencies: map[int64][]int64{3: {2}},
		ActiveRuns:   1,
		MaxParallel:  3,
	}

	frontier := ReadyFrontier(snapshot)
	if len(frontier) != 2 || frontier[0].IssueID != 1 || frontier[1].IssueID != 3 {
		t.Fatalf("frontier = %#v, want tickets 1 then 3", frontier)
	}

	// Map iteration and the input order must not affect the result.
	snapshot.Tickets[0], snapshot.Tickets[2] = snapshot.Tickets[2], snapshot.Tickets[0]
	reordered := ReadyFrontier(snapshot)
	if len(reordered) != len(frontier) || reordered[0].IssueID != frontier[0].IssueID || reordered[1].IssueID != frontier[1].IssueID {
		t.Fatalf("reordered frontier = %#v, want %#v", reordered, frontier)
	}
}

func TestReadyFrontierReturnsNoTicketWhenCapacityIsFull(t *testing.T) {
	snapshot := FrontierSnapshot{
		PlanState:   StateActive,
		Tickets:     []FrontierTicket{{IssueID: 1, Number: 1}},
		ActiveRuns:  2,
		MaxParallel: 2,
	}
	if frontier := ReadyFrontier(snapshot); len(frontier) != 0 {
		t.Fatalf("frontier = %#v, want empty", frontier)
	}
}
