package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	PlanLabel   = "workflow:plan"
	TicketLabel = "workflow:ticket"
	ActiveLabel = "workflow:active"

	ProjectionStart = "<!-- workflow:status:start -->"
	ProjectionEnd   = "<!-- workflow:status:end -->"
)

var (
	ErrInvalidPlan       = errors.New("invalid delivery plan")
	ErrIncompletePublish = fmt.Errorf("%w: native sub-issue is not a workflow ticket", ErrInvalidPlan)
	ErrUnknownBlocker    = fmt.Errorf("%w: blocker is outside the plan", ErrInvalidPlan)
	ErrCycle             = fmt.Errorf("%w: dependency cycle", ErrInvalidPlan)
	ErrDuplicateNode     = fmt.Errorf("%w: duplicate node", ErrInvalidPlan)
	ErrMalformedStatus   = errors.New("malformed workflow status projection")
)

// Issue is the read model needed to activate a Delivery Plan. IDs, rather
// than issue numbers, are used as graph keys because they are immutable.
type Issue struct {
	ID        int64
	NodeID    string
	Number    int64
	Title     string
	Body      string
	State     string
	Labels    []string
	UpdatedAt string
	Delivered bool
}

func (i Issue) HasLabel(label string) bool {
	for _, candidate := range i.Labels {
		if candidate == label {
			return true
		}
	}
	return false
}

func (i Issue) IsTicket() bool { return i.HasLabel(TicketLabel) }

func (i Issue) IsPlanRoot() bool {
	return i.HasLabel(PlanLabel) && !i.HasLabel(TicketLabel)
}

func (i Issue) IsDelivered() bool {
	return i.Delivered || i.HasLabel("workflow:delivered")
}

// Snapshot is one read of the GitHub Plan Root and its native graph. Children
// contains every native sub-issue, including untyped ones, so incomplete
// publication cannot be mistaken for an intentionally empty ticket set.
type Snapshot struct {
	Repository string
	Root       Issue
	Children   []Issue
	BlockedBy  map[int64][]Issue // blocked issue ID -> blocker issues
}

func (s Snapshot) Tickets() []Issue {
	tickets := make([]Issue, 0, len(s.Children))
	for _, child := range s.Children {
		if child.IsTicket() {
			tickets = append(tickets, child)
		}
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].ID < tickets[j].ID })
	return tickets
}

// Validate applies the admission boundary before anything is written to the
// runtime store. A valid plan has at least one typed child and a closed
// blocker must already carry the derived Delivered fact.
func (s Snapshot) Validate() error {
	if s.Repository == "" || s.Root.ID == 0 || s.Root.Number == 0 {
		return fmt.Errorf("%w: repository and root identity are required", ErrInvalidPlan)
	}
	if !s.Root.IsPlanRoot() {
		return fmt.Errorf("%w: root must have %q and must not have %q", ErrInvalidPlan, PlanLabel, TicketLabel)
	}
	if len(s.Children) == 0 {
		return fmt.Errorf("%w: plan has no native sub-issues", ErrIncompletePublish)
	}

	byID := make(map[int64]Issue, len(s.Children))
	byNumber := make(map[int64]struct{}, len(s.Children))
	for _, child := range s.Children {
		if child.ID == 0 || child.Number == 0 {
			return fmt.Errorf("%w: child identity is incomplete", ErrInvalidPlan)
		}
		if child.ID == s.Root.ID || child.Number == s.Root.Number {
			return fmt.Errorf("%w: plan root cannot be an executable ticket", ErrInvalidPlan)
		}
		if _, exists := byID[child.ID]; exists {
			return fmt.Errorf("%w: issue id %d", ErrDuplicateNode, child.ID)
		}
		if _, exists := byNumber[child.Number]; exists {
			return fmt.Errorf("%w: issue number %d", ErrDuplicateNode, child.Number)
		}
		byID[child.ID] = child
		byNumber[child.Number] = struct{}{}
		if !child.IsTicket() {
			return fmt.Errorf("%w: issue #%d", ErrIncompletePublish, child.Number)
		}
	}

	edges := make(map[[2]int64]struct{})
	adjacency := make(map[int64][]int64, len(byID))
	for blockedID, blockers := range s.BlockedBy {
		if _, ok := byID[blockedID]; !ok {
			return fmt.Errorf("%w: blocked issue id %d", ErrUnknownBlocker, blockedID)
		}
		for _, blocker := range blockers {
			if blocker.ID == 0 || blocker.Number == 0 {
				return fmt.Errorf("%w: blocker identity is incomplete", ErrInvalidPlan)
			}
			if blocker.ID == s.Root.ID || blocker.Number == s.Root.Number {
				return fmt.Errorf("%w: root cannot be a blocker", ErrUnknownBlocker)
			}
			if _, ok := byID[blocker.ID]; !ok {
				return fmt.Errorf("%w: issue #%d", ErrUnknownBlocker, blocker.Number)
			}
			if blocker.State == "closed" && !blocker.IsDelivered() {
				return fmt.Errorf("%w: closed blocker #%d is not Delivered", ErrInvalidPlan, blocker.Number)
			}
			edge := [2]int64{blockedID, blocker.ID}
			if _, exists := edges[edge]; exists {
				return fmt.Errorf("%w: %d blocked by %d", ErrDuplicateNode, blockedID, blocker.ID)
			}
			edges[edge] = struct{}{}
			adjacency[blockedID] = append(adjacency[blockedID], blocker.ID)
		}
	}
	if hasCycle(byID, adjacency) {
		return ErrCycle
	}
	return nil
}

func hasCycle(nodes map[int64]Issue, adjacency map[int64][]int64) bool {
	const (
		unseen   = 0
		visiting = 1
		visited  = 2
	)
	state := make(map[int64]int, len(nodes))
	var visit func(int64) bool
	visit = func(node int64) bool {
		if state[node] == visiting {
			return true
		}
		if state[node] == visited {
			return false
		}
		state[node] = visiting
		for _, blocker := range adjacency[node] {
			if visit(blocker) {
				return true
			}
		}
		state[node] = visited
		return false
	}
	for id := range nodes {
		if visit(id) {
			return true
		}
	}
	return false
}

// Fingerprint deliberately excludes the Plan Root's status block. Updating
// the projection changes GitHub's updated_at timestamp, but must not create a
// new immutable plan version on a retry after a process restart.
func (s Snapshot) Fingerprint() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	tickets := s.Tickets()
	deps := make([][2]int64, 0)
	for blocked, blockers := range s.BlockedBy {
		for _, blocker := range blockers {
			deps = append(deps, [2]int64{blocked, blocker.ID})
		}
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i][0] == deps[j][0] {
			return deps[i][1] < deps[j][1]
		}
		return deps[i][0] < deps[j][0]
	})
	type issueFingerprint struct {
		ID, Number  int64
		Title, Body string
		Labels      []string
	}
	canonical := struct {
		Repository   string
		Root         issueFingerprint
		Tickets      []issueFingerprint
		Dependencies [][2]int64
	}{
		Repository:   s.Repository,
		Root:         issueFingerprint{ID: s.Root.ID, Number: s.Root.Number, Title: s.Root.Title, Body: stripProjection(s.Root.Body), Labels: sortedStrings(contractLabels(s.Root.Labels))},
		Dependencies: deps,
	}
	for _, ticket := range tickets {
		canonical.Tickets = append(canonical.Tickets, issueFingerprint{ID: ticket.ID, Number: ticket.Number, Title: ticket.Title, Body: ticket.Body, Labels: sortedStrings(contractLabels(ticket.Labels))})
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

func sortedStrings(values []string) []string {
	copyOf := append([]string(nil), values...)
	sort.Strings(copyOf)
	return copyOf
}

func contractLabels(labels []string) []string {
	filtered := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != ActiveLabel && label != "workflow:delivered" {
			filtered = append(filtered, label)
		}
	}
	return filtered
}

func stripProjection(body string) string {
	start := strings.Index(body, ProjectionStart)
	if start < 0 {
		return body
	}
	end := strings.Index(body[start+len(ProjectionStart):], ProjectionEnd)
	if end < 0 {
		return body
	}
	end += start + len(ProjectionStart)
	end += len(ProjectionEnd)
	return strings.TrimRight(body[:start], "\r\n") + strings.TrimLeft(body[end:], "\r\n")
}
