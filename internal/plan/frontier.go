package plan

import "sort"

const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateActive    = "active"
	StateDelivered = "delivered"
)

// FrontierTicket is the small immutable/runtime join needed by the scheduler.
// Owner is empty when the ticket has no live Ticket Session ownership.
type FrontierTicket struct {
	IssueID   int64
	Number    int64
	Title     string
	Delivered bool
	Owner     string
	Rank      int
}

type FrontierSnapshot struct {
	VersionID    string
	PlanState    string
	Tickets      []FrontierTicket
	Dependencies map[int64][]int64
	ActiveRuns   int
	MaxParallel  int
}

// ReadyFrontier computes the deterministic dispatch set from one persistent
// snapshot. The result is only a preview: ClaimReady must repeat these checks
// inside its write transaction before granting execution ownership.
func ReadyFrontier(snapshot FrontierSnapshot) []FrontierTicket {
	if snapshot.PlanState != StateActive || snapshot.MaxParallel <= snapshot.ActiveRuns {
		return nil
	}
	byID := make(map[int64]FrontierTicket, len(snapshot.Tickets))
	for _, ticket := range snapshot.Tickets {
		byID[ticket.IssueID] = ticket
	}
	ranks := make(map[int64]int, len(byID))
	var rank func(int64, map[int64]bool) int
	rank = func(issueID int64, visiting map[int64]bool) int {
		if value, ok := ranks[issueID]; ok {
			return value
		}
		if visiting[issueID] {
			return 0
		}
		visiting[issueID] = true
		best := 0
		for _, blockerID := range snapshot.Dependencies[issueID] {
			candidate := rank(blockerID, visiting) + 1
			if candidate > best {
				best = candidate
			}
		}
		delete(visiting, issueID)
		ranks[issueID] = best
		return best
	}
	for issueID := range byID {
		rank(issueID, make(map[int64]bool))
	}

	ready := make([]FrontierTicket, 0, len(snapshot.Tickets))
	for _, ticket := range snapshot.Tickets {
		if ticket.Delivered || ticket.Owner != "" {
			continue
		}
		allDelivered := true
		for _, blockerID := range snapshot.Dependencies[ticket.IssueID] {
			blocker, exists := byID[blockerID]
			if !exists || !blocker.Delivered {
				allDelivered = false
				break
			}
		}
		if allDelivered {
			ticket.Rank = ranks[ticket.IssueID]
			ready = append(ready, ticket)
		}
	}

	// Issue numbers are the human-visible stable order; immutable IDs break a
	// pathological number collision without depending on input/map iteration.
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Rank == 0 && ready[j].Rank != 0 {
			return true
		}
		if ready[i].Rank != 0 && ready[j].Rank == 0 {
			return false
		}
		if ready[i].Rank != ready[j].Rank {
			return ready[i].Rank < ready[j].Rank
		}
		if ready[i].Number == ready[j].Number {
			return ready[i].IssueID < ready[j].IssueID
		}
		return ready[i].Number < ready[j].Number
	})
	capacity := snapshot.MaxParallel - snapshot.ActiveRuns
	if capacity < len(ready) {
		ready = ready[:capacity]
	}
	return ready
}
