package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type Controller struct {
	Store        *store.Store
	Workspace    WorkspaceManager
	Runtime      worker.Runtime
	ImageDigest  string
	ToolVersions map[string]string
	NoMistakes   string
	GatewayURL   string
	Now          func() time.Time
}

func (c Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

type RunRequest struct {
	Claim            store.TicketClaim
	SourceRepository string
	Branch           string
	Prompt           string
	Publication      store.CandidatePublication
}

type Candidate struct {
	RunID            string
	SessionID        string
	CodexSessionID   string
	Commit           string
	StructuredOutput []byte
	PushOutboxKey    string
	PROutboxKey      string
}

func (c Controller) Run(ctx context.Context, request RunRequest) (Candidate, error) {
	if c.Store == nil || c.Runtime == nil {
		return Candidate{}, errors.New("agent controller dependencies are incomplete")
	}
	if request.Claim.SessionID == "" || request.Claim.RunID == "" || request.Prompt == "" {
		return Candidate{}, store.ErrInvalidClaim
	}
	session, err := c.Store.TicketSession(ctx, request.Claim.VersionID, request.Claim.TicketID)
	if err != nil {
		return Candidate{}, err
	}
	ws, err := c.Workspace.ensure(ctx, session.SessionID, request.SourceRepository, request.Branch)
	if err != nil {
		return Candidate{}, err
	}
	identity := session.AgentIdentity
	if identity == "" {
		identity = "agent-" + session.SessionID
	}
	session, err = c.Store.BindAgent(ctx, store.AgentBinding{SessionID: session.SessionID, AgentIdentity: identity, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch})
	if err != nil {
		return Candidate{}, err
	}
	baseCommit, currentBranch, clean, err := c.Workspace.status(ctx, ws)
	if err != nil {
		return Candidate{}, err
	}
	if currentBranch != ws.Branch {
		return c.failRun(ctx, request, ws, session, baseCommit, "workspace branch changed before the worker started", "")
	}
	if !clean {
		return c.failRun(ctx, request, ws, session, baseCommit, "workspace was not clean before the worker started", "")
	}
	noMistakes := c.NoMistakes
	if noMistakes == "" {
		noMistakes = "no-mistakes"
	}
	command := []string{noMistakes, "axi", "run", "--intent", request.Prompt}
	environment := map[string]string{
		"CODEX_HOME":                 ws.CodexState,
		"NO_MISTAKES_DELIVERY_CYCLE": session.SessionID,
	}
	if c.GatewayURL != "" {
		environment["NO_MISTAKES_GATEWAY_URL"] = c.GatewayURL
	}
	spec := worker.Spec{
		Command: command, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch,
		AgentIdentity: session.AgentIdentity, ImageDigest: c.ImageDigest, ToolVersions: c.ToolVersions,
		Environment: environment,
		Mounts:      []worker.Mount{{Source: ws.Path, Target: "/workspace"}, {Source: ws.CodexState, Target: "/codex-state"}},
		ExtraHosts:  []string{worker.GatewayHostMapping},
	}
	if err := spec.Validate(); err != nil {
		return Candidate{}, err
	}
	result, runErr := c.Runtime.Run(ctx, spec)
	handoffCtx := context.WithoutCancel(ctx)
	if err := c.Store.RecordWorkerAudit(handoffCtx, store.WorkerAudit{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, ContainerID: result.ContainerID, ImageDigest: spec.ImageDigest, Mounts: spec.Mounts, ExtraHosts: spec.ExtraHosts, ToolVersions: spec.ToolVersions}); err != nil {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, err.Error(), string(result.Output))
	}
	codexSessionID, _ := parseSessionID(result.Output, session.CodexSessionID)
	if codexSessionID != "" && codexSessionID != session.CodexSessionID {
		if err := c.Store.RecordCodexSession(handoffCtx, request.Claim.RunID, request.Claim.LeaseToken, codexSessionID); err != nil {
			return c.failRun(handoffCtx, request, ws, session, baseCommit, err.Error(), string(result.Output))
		}
		session.CodexSessionID = codexSessionID
	}
	if runErr != nil || result.ExitCode != 0 {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, errorText(runErr, result.ExitCode), string(result.Output))
	}
	commit, currentBranch, clean, err := c.Workspace.status(handoffCtx, ws)
	if err != nil {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, err.Error(), string(result.Output))
	}
	if currentBranch != ws.Branch {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, fmt.Sprintf("worker changed workspace branch to %q", currentBranch), string(result.Output))
	}
	if !clean {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, "worker completed with a dirty workspace", string(result.Output))
	}
	codexSessionID, structured, err := parseOutput(result.Output, session.CodexSessionID)
	if err != nil {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, err.Error(), string(result.Output))
	}
	if commit == baseCommit {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, "worker produced no new commit", string(result.Output))
	}
	var candidateOutput struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(structured, &candidateOutput); err != nil {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, "Codex structured result is invalid JSON", string(result.Output))
	}
	if candidateOutput.Commit != "" && candidateOutput.Commit != commit {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, "Codex structured result names a different commit", string(result.Output))
	}
	publication := request.Publication
	if publication.Body == "" {
		publication.Body = candidateSummary(structured)
	}
	handoff, err := c.Store.AcceptCandidate(handoffCtx, store.CandidateRevision{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, CodexSessionID: codexSessionID, CommitSHA: commit, StructuredOutput: structured, Now: c.now(), Publication: publication})
	if err != nil {
		return c.failRun(handoffCtx, request, ws, session, baseCommit, err.Error(), string(result.Output))
	}
	return Candidate{RunID: request.Claim.RunID, SessionID: session.SessionID, CodexSessionID: codexSessionID, Commit: commit, StructuredOutput: structured, PushOutboxKey: handoff.PushOutboxKey, PROutboxKey: handoff.PROutboxKey}, nil
}

func (c Controller) failRun(ctx context.Context, request RunRequest, ws workspace, session store.TicketSession, baseCommit, reason, output string) (Candidate, error) {
	diagnostic, diagnosticErr := c.Workspace.diagnostic(ctx, ws, request.Claim.RunID, baseCommit, output, reason)
	restoreCommit := session.AcceptedCommit
	if restoreCommit == "" {
		restoreCommit = baseCommit
	}
	restoreErr := c.Workspace.restore(ctx, ws, restoreCommit)
	var recordErr error
	if diagnosticErr == nil {
		recordErr = c.Store.RecordRunFailure(ctx, store.RunFailure{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, DiagnosticsPath: diagnostic, Error: reason, Now: c.now()})
	}
	return Candidate{}, errors.Join(errors.New(reason), diagnosticErr, restoreErr, recordErr)
}

func errorText(err error, exitCode int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("worker exited with status %d", exitCode)
}

func parseOutput(output []byte, existing string) (string, []byte, error) {
	sessionID, _ := parseSessionID(output, existing)
	var structured []byte
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var value struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			continue
		}
		if value.Type == "item.completed" && value.Item.Type == "agent_message" {
			candidate := []byte(strings.TrimSpace(value.Item.Text))
			if validateStructuredOutput(candidate) == nil {
				structured = append([]byte(nil), candidate...)
			}
		}
	}
	if sessionID == "" {
		sessionID = existing
	}
	if sessionID == "" || len(structured) == 0 {
		return "", nil, errors.New("Codex output did not contain a session and structured result")
	}
	return sessionID, structured, nil
}

func validateStructuredOutput(output []byte) error {
	var value struct {
		Summary any   `json:"summary"`
		Commit  any   `json:"commit"`
		Tests   []any `json:"tests"`
	}
	if len(output) == 0 || json.Unmarshal(output, &value) != nil {
		return errors.New("structured result is not a JSON object")
	}
	if summary, ok := value.Summary.(string); !ok || strings.TrimSpace(summary) == "" {
		return errors.New("structured result requires a nonempty summary")
	}
	if value.Commit != nil {
		if _, ok := value.Commit.(string); !ok {
			return errors.New("structured result commit must be a string")
		}
	}
	for _, test := range value.Tests {
		if _, ok := test.(string); !ok {
			return errors.New("structured result tests must be strings")
		}
	}
	return nil
}

func candidateSummary(output []byte) string {
	var result struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(output, &result); err != nil || strings.TrimSpace(result.Summary) == "" {
		return "Worker completed candidate delivery."
	}
	return result.Summary
}

func parseSessionID(output []byte, existing string) (string, error) {
	var sessionID string
	for _, line := range strings.Split(string(output), "\n") {
		var value map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &value) != nil {
			continue
		}
		for _, key := range []string{"session_id", "sessionId", "thread_id", "threadId"} {
			if candidate, ok := value[key].(string); ok && candidate != "" {
				sessionID = candidate
			}
		}
	}
	if sessionID == "" {
		sessionID = existing
	}
	if sessionID == "" {
		return "", errors.New("Codex output did not contain a session")
	}
	return sessionID, nil
}
