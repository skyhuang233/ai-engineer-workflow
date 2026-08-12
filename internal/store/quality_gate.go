package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
)

// QualityGate is the structured result emitted by the Delivery Controller
// when it cannot make a product decision autonomously.
type QualityGate struct {
	Source         string
	GateID         string
	FindingID      string
	Action         string
	Reason         string
	AllowedAnswers []string
}

const (
	QualityGateAskUser = "ask-user"
	QualityGateSkip    = "skip"
)

// PauseDeliveryControllerForQualityGate atomically releases the active
// Delivery Controller lease and opens one question for an unresolved gate
// fingerprint. It deliberately does not create a generic Needs Attention
// question: the gate answer is the only valid way to resume this delivery.
func (s *Store) PauseDeliveryControllerForQualityGate(ctx context.Context, claim TicketClaim, gate QualityGate, now time.Time) (WorkflowQuestion, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if claim.VersionID == "" || claim.TicketID == 0 || claim.RunID == "" || claim.LeaseToken == "" || claim.LeaseGeneration <= 0 {
		return WorkflowQuestion{}, ErrInvalidClaim
	}
	gate = normalizeQualityGate(gate)
	if err := gate.validate(); err != nil {
		return WorkflowQuestion{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowQuestion{}, err
	}
	defer tx.Rollback()
	claim, err = resolveRunBoundClaimVersion(ctx, tx, claim)
	if err != nil {
		return WorkflowQuestion{}, err
	}
	var sessionID, repository, expiresText string
	var ticketNumber int64
	err = tx.QueryRowContext(ctx, `SELECT s.session_id, p.repository, t.issue_number, l.expires_at
FROM ticket_sessions s
JOIN worker_runs r ON r.run_id = s.current_run_id
JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation
JOIN plan_versions v ON v.version_id = s.version_id
JOIN plans p ON p.id = v.plan_id
JOIN plan_tickets t ON t.version_id = s.version_id AND t.issue_id = s.issue_id
WHERE s.version_id = ? AND s.issue_id = ? AND r.run_id = ? AND r.run_kind = ? AND r.state = ?
  AND l.lease_token = ? AND l.generation = ? AND l.state = ?`,
		claim.VersionID, claim.TicketID, claim.RunID, RunDelivery, RunRunning, claim.LeaseToken, claim.LeaseGeneration, LeaseActive).
		Scan(&sessionID, &repository, &ticketNumber, &expiresText)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowQuestion{}, ErrInvalidClaim
	}
	if err != nil {
		return WorkflowQuestion{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return WorkflowQuestion{}, err
	}
	if !expiresAt.After(now) {
		return WorkflowQuestion{}, ErrInvalidClaim
	}
	if err := requireDeliveryRunTerminalizationTx(ctx, tx, claim.RunID, claim.LeaseGeneration); err != nil {
		return WorkflowQuestion{}, err
	}
	fingerprint := qualityGateFingerprint(gate)
	var question WorkflowQuestion
	err = tx.QueryRowContext(ctx, `SELECT q.question_id, q.prompt, q.state, q.answer
FROM quality_gate_questions gate
JOIN workflow_questions q ON q.question_id = gate.question_id
WHERE gate.version_id = ? AND gate.issue_id = ? AND gate.fingerprint = ? AND q.state = 'open'`, claim.VersionID, claim.TicketID, fingerprint).
		Scan(&question.ID, &question.Prompt, &question.State, &question.Answer)
	if errors.Is(err, sql.ErrNoRows) {
		var generation int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM workflow_questions WHERE repository = ? AND version_id = ? AND issue_id = ? AND kind = 'quality_gate'`, repository, claim.VersionID, claim.TicketID).Scan(&generation); err != nil {
			return WorkflowQuestion{}, err
		}
		question.ID = fmt.Sprintf("quality-gate-%s-g%d", fingerprint[:16], generation)
		question.Prompt = qualityGatePrompt(ticketNumber, question.ID, gate)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_questions(question_id, repository, version_id, issue_id, kind, generation, prompt, state, created_at)
VALUES (?, ?, ?, ?, 'quality_gate', ?, ?, 'open', ?)`, question.ID, repository, claim.VersionID, claim.TicketID, generation, question.Prompt, formatTimestamp(now)); err != nil {
			return WorkflowQuestion{}, err
		}
		answers, err := json.Marshal(gate.AllowedAnswers)
		if err != nil {
			return WorkflowQuestion{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO quality_gate_questions(question_id, version_id, issue_id, session_id, delivery_run_id, source, gate_id, finding_id, action, reason, fingerprint, allowed_answers_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, question.ID, claim.VersionID, claim.TicketID, sessionID, claim.RunID, gate.Source, gate.GateID, gate.FindingID, gate.Action, gate.Reason, fingerprint, string(answers)); err != nil {
			return WorkflowQuestion{}, err
		}
	} else if err != nil {
		return WorkflowQuestion{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_runs SET state = 'succeeded', finished_at = ? WHERE run_id = ? AND state = ?`, formatTimestamp(now), claim.RunID, RunRunning); err != nil {
		return WorkflowQuestion{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET state = 'revoked' WHERE run_id = ? AND lease_token = ? AND state = ?`, claim.RunID, claim.LeaseToken, LeaseActive); err != nil {
		return WorkflowQuestion{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ticket_runtime SET state = ?, updated_at = ? WHERE version_id = ? AND issue_id = ? AND delivered = 0`, plan.StateNeedsAttention, formatTimestamp(now), claim.VersionID, claim.TicketID); err != nil {
		return WorkflowQuestion{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowQuestion{}, err
	}
	question.Repository, question.VersionID, question.IssueID, question.Kind, question.State, question.TicketNumber = repository, claim.VersionID, claim.TicketID, "quality_gate", "open", ticketNumber
	return question, nil
}

// DeliveryQualityGateAnswer returns the one answered gate tied to this Ticket
// Session. It is intentionally read on every retry so a crash between the
// answer and controller completion cannot silently lose the human decision.
func (s *Store) DeliveryQualityGateAnswer(ctx context.Context, sessionID string) (QualityGate, string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return QualityGate{}, "", ErrInvalidClaim
	}
	var gate QualityGate
	var allowed, answer string
	err := s.db.QueryRowContext(ctx, `SELECT g.source, g.gate_id, g.finding_id, g.action, g.reason, g.allowed_answers_json, q.answer
FROM quality_gate_questions g
JOIN workflow_questions q ON q.question_id = g.question_id
WHERE g.session_id = ? AND q.state = 'answered' AND g.consumed_at = ''
ORDER BY q.answered_at DESC, q.question_id DESC LIMIT 1`, sessionID).
		Scan(&gate.Source, &gate.GateID, &gate.FindingID, &gate.Action, &gate.Reason, &allowed, &answer)
	if errors.Is(err, sql.ErrNoRows) {
		return QualityGate{}, "", ErrNotFound
	}
	if err != nil {
		return QualityGate{}, "", err
	}
	if err := json.Unmarshal([]byte(allowed), &gate.AllowedAnswers); err != nil {
		return QualityGate{}, "", err
	}
	return gate, answer, nil
}

func normalizeQualityGate(gate QualityGate) QualityGate {
	gate.Source = strings.TrimSpace(gate.Source)
	if gate.Source == "" {
		gate.Source = "no-mistakes"
	}
	gate.GateID = strings.TrimSpace(gate.GateID)
	gate.FindingID = strings.TrimSpace(gate.FindingID)
	gate.Action = strings.TrimSpace(strings.ToLower(gate.Action))
	gate.Reason = strings.TrimSpace(gate.Reason)
	answers := make([]string, 0, len(gate.AllowedAnswers))
	for _, answer := range gate.AllowedAnswers {
		if answer = strings.TrimSpace(answer); answer != "" && !slices.Contains(answers, answer) {
			answers = append(answers, answer)
		}
	}
	if len(answers) == 0 && gate.Action == QualityGateSkip {
		answers = []string{"skip"}
	}
	gate.AllowedAnswers = answers
	return gate
}

func (g QualityGate) validate() error {
	if g.GateID == "" || g.Action == "" || g.Reason == "" || len(g.AllowedAnswers) == 0 {
		return ErrInvalidClaim
	}
	if g.Action != QualityGateAskUser && g.Action != QualityGateSkip {
		return ErrInvalidClaim
	}
	return nil
}

func qualityGateFingerprint(gate QualityGate) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{gate.Source, gate.FindingID, gate.Action, gate.Reason, strings.Join(gate.AllowedAnswers, "\x00")}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func qualityGatePrompt(ticketNumber int64, questionID string, gate QualityGate) string {
	return fmt.Sprintf("Quality gate %s from %s paused ticket #%d. Finding: %s. Reason: %s. Allowed answers: %s. Reply with workflow-answer:%s: <one allowed answer> to resume the same Ticket Agent and gate.", gate.GateID, gate.Source, ticketNumber, gate.FindingID, gate.Reason, strings.Join(gate.AllowedAnswers, ", "), questionID)
}
