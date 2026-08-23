package plan

import (
	"fmt"
	"sort"
	"strings"
)

type Projection struct {
	VersionID      string             `json:"version_id"`
	State          string             `json:"state"`
	DispatchPaused string             `json:"dispatch_paused,omitempty"`
	Tickets        []ProjectionTicket `json:"tickets"`
	Questions      []WorkflowQuestion `json:"questions,omitempty"`
}

type WorkflowQuestion struct {
	ID           string  `json:"id"`
	Prompt       string  `json:"prompt"`
	Repository   string  `json:"repository,omitempty"`
	PlanNumber   int64   `json:"plan_number,omitempty"`
	PlanNumbers  []int64 `json:"plan_numbers,omitempty"`
	TicketNumber int64   `json:"ticket_number,omitempty"`
	PullRequest  int64   `json:"pull_request,omitempty"`
	Commit       string  `json:"commit,omitempty"`
	Finding      string  `json:"finding,omitempty"`
	Diagnostics  string  `json:"diagnostics,omitempty"`
	Evidence     string  `json:"evidence,omitempty"`
}

type ProjectionTicket struct {
	Number          int64   `json:"number"`
	Title           string  `json:"title"`
	State           string  `json:"state,omitempty"`
	Owner           string  `json:"owner,omitempty"`
	SessionID       string  `json:"session_id,omitempty"`
	RunID           string  `json:"run_id,omitempty"`
	LeaseGeneration int64   `json:"lease_generation,omitempty"`
	PullRequest     int64   `json:"pull_request,omitempty"`
	Revision        string  `json:"revision,omitempty"`
	GateResult      string  `json:"gate_result,omitempty"`
	LastActivity    string  `json:"last_activity,omitempty"`
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
	if projection.DispatchPaused != "" {
		fmt.Fprintf(&b, "- new dispatches: paused — %s\n\n", escapeCell(projection.DispatchPaused))
	}
	if len(tickets) == 0 {
		b.WriteString("No executable tickets.\n")
	} else {
		runtime := false
		for _, ticket := range tickets {
			if ticket.State != "" || ticket.Owner != "" || ticket.SessionID != "" || ticket.RunID != "" || ticket.LeaseGeneration != 0 || ticket.PullRequest != 0 || ticket.Revision != "" || ticket.GateResult != "" || ticket.LastActivity != "" {
				runtime = true
				break
			}
		}
		if runtime {
			b.WriteString("| Ticket | State | Owner | Session | Run | Lease | Pull request | Revision | Gate | Last activity | Blocked by |\n| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
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
				fmt.Fprintf(&b, "| #%d %s | %s | %s | %s | %s | %d | %s | %s | %s | %s | %s |\n", ticket.Number, escapeCell(ticket.Title), escapeCell(ticket.State), escapeCell(ticket.Owner), escapeCell(ticket.SessionID), escapeCell(ticket.RunID), ticket.LeaseGeneration, pullRequestReference(ticket.PullRequest), escapeCell(shortRevision(ticket.Revision)), escapeCell(ticket.GateResult), escapeCell(ticket.LastActivity), strings.Join(blockers, ", "))
			} else {
				fmt.Fprintf(&b, "| #%d %s | %s |\n", ticket.Number, escapeCell(ticket.Title), strings.Join(blockers, ", "))
			}
		}
	}
	if len(projection.Questions) > 0 {
		b.WriteString("\n### Workflow Inbox\n\nPending human decisions are listed in the repository Workflow Inbox.\n")
	}
	fmt.Fprintf(&b, "%s", ProjectionEnd)
	return b.String()
}

func RenderWorkflowInbox(questions []WorkflowQuestion) string {
	questions = append([]WorkflowQuestion(nil), questions...)
	sort.Slice(questions, func(i, j int) bool { return questions[i].ID < questions[j].ID })
	var b strings.Builder
	b.WriteString("# Workflow Inbox\n\nReply with `workflow-answer:<question-id>: <answer>`.\n")
	if len(questions) == 0 {
		b.WriteString("\nNo open workflow questions.\n")
		return b.String()
	}
	b.WriteString("\n## Open questions\n\n")
	for _, question := range questions {
		fmt.Fprintf(&b, "- `%s`: %s\n", question.ID, escapeCell(question.Prompt))
		if context := workflowQuestionContext(question); len(context) > 0 {
			fmt.Fprintf(&b, "  - %s\n", strings.Join(context, " | "))
		}
	}
	return b.String()
}

func workflowQuestionContext(question WorkflowQuestion) []string {
	if question.Repository == "" {
		return nil
	}
	base := "https://github.com/" + question.Repository
	context := make([]string, 0, 7)
	if len(question.PlanNumbers) > 0 {
		for _, number := range question.PlanNumbers {
			context = append(context, fmt.Sprintf("[plan/spec #%d](%s/issues/%d)", number, base, number))
		}
	} else if question.PlanNumber > 0 {
		context = append(context, fmt.Sprintf("[plan/spec #%d](%s/issues/%d)", question.PlanNumber, base, question.PlanNumber))
	}
	if question.TicketNumber > 0 {
		context = append(context, fmt.Sprintf("[ticket #%d](%s/issues/%d)", question.TicketNumber, base, question.TicketNumber))
	}
	if question.PullRequest > 0 {
		context = append(context, fmt.Sprintf("[PR #%d](%s/pull/%d)", question.PullRequest, base, question.PullRequest))
	}
	if question.Commit != "" {
		context = append(context, fmt.Sprintf("[commit %s](%s/commit/%s)", shortRevision(question.Commit), base, question.Commit))
	}
	if question.Finding != "" {
		context = append(context, "finding: `"+question.Finding+"`")
	}
	if question.Diagnostics != "" {
		context = append(context, "diagnostics: `"+question.Diagnostics+"`")
	}
	if question.Evidence != "" {
		context = append(context, "evidence: `"+question.Evidence+"`")
	}
	return context
}

func pullRequestReference(number int64) string {
	if number == 0 {
		return ""
	}
	return fmt.Sprintf("#%d", number)
}

func shortRevision(revision string) string {
	if len(revision) <= 12 {
		return revision
	}
	return revision[:12]
}

func escapeCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\r", " ", "\n", " ").Replace(value)
}
