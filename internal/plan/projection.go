package plan

import (
	"fmt"
	"sort"
	"strings"
)

type Projection struct {
	VersionID string             `json:"version_id"`
	State     string             `json:"state"`
	Tickets   []ProjectionTicket `json:"tickets"`
}

type ProjectionTicket struct {
	Number          int64   `json:"number"`
	Title           string  `json:"title"`
	State           string  `json:"state,omitempty"`
	Owner           string  `json:"owner,omitempty"`
	SessionID       string  `json:"session_id,omitempty"`
	RunID           string  `json:"run_id,omitempty"`
	LeaseGeneration int64   `json:"lease_generation,omitempty"`
	Blockers        []int64 `json:"blockers,omitempty"`
}

// RenderProjection replaces only the control plane's hidden block. Human
// maintained specification text before and after the block is preserved byte
// for byte (apart from the separator needed when appending a new block).
func RenderProjection(body string, projection Projection) (string, error) {
	block := renderBlock(projection)
	if strings.Count(body, ProjectionStart) > 1 || strings.Count(body, ProjectionEnd) > 1 {
		return "", ErrMalformedStatus
	}
	start := strings.Index(body, ProjectionStart)
	endMarker := strings.Index(body, ProjectionEnd)
	if (start >= 0) != (endMarker >= 0) || endMarker >= 0 && endMarker < start {
		return "", ErrMalformedStatus
	}
	if start >= 0 {
		end := endMarker + len(ProjectionEnd)
		return body[:start] + block + body[end:], nil
	}
	if body == "" {
		return block, nil
	}
	return strings.TrimRight(body, "\r\n") + "\n\n" + block, nil
}

func renderBlock(projection Projection) string {
	tickets := append([]ProjectionTicket(nil), projection.Tickets...)
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].Number < tickets[j].Number })
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", ProjectionStart)
	fmt.Fprintf(&b, "### Control Plane\n\n- state: `%s`\n- plan version: `%s`\n\n", projection.State, projection.VersionID)
	if len(tickets) == 0 {
		b.WriteString("No executable tickets.\n")
	} else {
		runtime := false
		for _, ticket := range tickets {
			if ticket.State != "" || ticket.Owner != "" || ticket.SessionID != "" || ticket.RunID != "" || ticket.LeaseGeneration != 0 {
				runtime = true
				break
			}
		}
		if runtime {
			b.WriteString("| Ticket | State | Owner | Session | Run | Lease | Blocked by |\n| --- | --- | --- | --- | --- | --- | --- |\n")
		} else {
			b.WriteString("| Ticket | Blocked by |\n| --- | --- |\n")
		}
		for _, ticket := range tickets {
			blockers := make([]string, len(ticket.Blockers))
			for i, blocker := range ticket.Blockers {
				blockers[i] = fmt.Sprintf("#%d", blocker)
			}
			sort.Strings(blockers)
			if runtime {
				fmt.Fprintf(&b, "| #%d %s | %s | %s | %s | %s | %d | %s |\n", ticket.Number, escapeCell(ticket.Title), escapeCell(ticket.State), escapeCell(ticket.Owner), escapeCell(ticket.SessionID), escapeCell(ticket.RunID), ticket.LeaseGeneration, strings.Join(blockers, ", "))
			} else {
				fmt.Fprintf(&b, "| #%d %s | %s |\n", ticket.Number, escapeCell(ticket.Title), strings.Join(blockers, ", "))
			}
		}
	}
	fmt.Fprintf(&b, "%s", ProjectionEnd)
	return b.String()
}

func escapeCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\r", " ", "\n", " ").Replace(value)
}
