package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
}

type RunRequest struct {
	Claim            store.TicketClaim
	SourceRepository string
	Branch           string
	Prompt           string
}

type Candidate struct {
	RunID            string
	SessionID        string
	CodexSessionID   string
	Commit           string
	StructuredOutput []byte
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
	if err := os.WriteFile(c.Workspace.schemaPath(ws.CodexState), []byte(outputSchema), 0o600); err != nil {
		return Candidate{}, err
	}
	baseCommit, clean, err := c.Workspace.status(ctx, ws)
	if err != nil {
		return Candidate{}, err
	}
	if !clean {
		return c.failRun(ctx, request, ws, session, baseCommit, "workspace was not clean before the worker started", "")
	}
	command := []string{"codex", "exec", "--json", "--output-schema", c.Workspace.schemaPath(ws.CodexState)}
	if session.CodexSessionID != "" {
		command = []string{"codex", "exec", "resume", session.CodexSessionID, "--json", "--output-schema", c.Workspace.schemaPath(ws.CodexState)}
	}
	command = append(command, request.Prompt)
	spec := worker.Spec{
		Command: command, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch,
		AgentIdentity: session.AgentIdentity, ImageDigest: c.ImageDigest, ToolVersions: c.ToolVersions,
		Environment: map[string]string{"CODEX_HOME": ws.CodexState},
		Mounts:      []worker.Mount{{Source: ws.Path, Target: "/workspace"}, {Source: ws.CodexState, Target: "/codex-state"}},
	}
	if err := spec.Validate(); err != nil {
		return Candidate{}, err
	}
	result, runErr := c.Runtime.Run(ctx, spec)
	if err := c.Store.RecordWorkerAudit(ctx, store.WorkerAudit{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, ContainerID: result.ContainerID, ImageDigest: spec.ImageDigest, Mounts: spec.Mounts, ToolVersions: spec.ToolVersions}); err != nil {
		return Candidate{}, err
	}
	codexSessionID, _ := parseSessionID(result.Output, session.CodexSessionID)
	if codexSessionID != "" && codexSessionID != session.CodexSessionID {
		if err := c.Store.RecordCodexSession(ctx, request.Claim.RunID, request.Claim.LeaseToken, codexSessionID); err != nil {
			return Candidate{}, err
		}
		session.CodexSessionID = codexSessionID
	}
	if runErr != nil || result.ExitCode != 0 {
		return c.failRun(ctx, request, ws, session, baseCommit, errorText(runErr, result.ExitCode), string(result.Output))
	}
	commit, clean, err := c.Workspace.status(ctx, ws)
	if err != nil {
		return Candidate{}, err
	}
	if !clean {
		return c.failRun(ctx, request, ws, session, baseCommit, "worker completed with a dirty workspace", string(result.Output))
	}
	codexSessionID, structured, err := parseOutput(result.Output, session.CodexSessionID)
	if err != nil {
		return c.failRun(ctx, request, ws, session, baseCommit, err.Error(), string(result.Output))
	}
	if commit == baseCommit {
		return c.failRun(ctx, request, ws, session, baseCommit, "worker produced no new commit", string(result.Output))
	}
	if err := c.Store.AcceptCandidate(ctx, store.CandidateRevision{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, CodexSessionID: codexSessionID, CommitSHA: commit, StructuredOutput: structured, Now: time.Now().UTC()}); err != nil {
		return Candidate{}, err
	}
	return Candidate{RunID: request.Claim.RunID, SessionID: session.SessionID, CodexSessionID: codexSessionID, Commit: commit, StructuredOutput: structured}, nil
}

func (c Controller) failRun(ctx context.Context, request RunRequest, ws workspace, session store.TicketSession, baseCommit, reason, output string) (Candidate, error) {
	diagnostic, err := c.Workspace.diagnostic(ctx, ws, request.Claim.RunID, output, reason)
	if err != nil {
		return Candidate{}, err
	}
	restoreCommit := session.AcceptedCommit
	if restoreCommit == "" {
		restoreCommit = baseCommit
	}
	if err := c.Workspace.restore(ctx, ws, restoreCommit); err != nil {
		return Candidate{}, err
	}
	if err := c.Store.RecordRunFailure(ctx, store.RunFailure{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, DiagnosticsPath: diagnostic, Error: reason}); err != nil {
		return Candidate{}, err
	}
	return Candidate{}, errors.New(reason)
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
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			continue
		}
		if _, ok := value["result"]; ok || value["type"] == "result" || value["type"] == "turn.completed" {
			structured = append([]byte(nil), []byte(line)...)
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

const outputSchema = `{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"},"commit":{"type":"string"},"tests":{"type":"array","items":{"type":"string"}}},"additionalProperties":true}`
