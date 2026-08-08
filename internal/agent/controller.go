package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	candidateoutput "github.com/skyhuang233/workflow/internal/candidate"
	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
	toon "github.com/toon-format/toon-go"
)

type Controller struct {
	Store            *store.Store
	Workspace        WorkspaceManager
	Runtime          worker.Runtime
	ImageDigest      string
	ToolVersions     map[string]string
	NoMistakes       string
	GatewayURL       string
	DeliveryLeaseTTL time.Duration
	Now              func() time.Time
}

func (c Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Controller) deliveryLeaseTTL() time.Duration {
	if c.DeliveryLeaseTTL > 0 {
		return c.DeliveryLeaseTTL
	}
	return 30 * time.Minute
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
}

const codexAuthenticationFailure = "Ticket Session Codex authentication cache is unavailable"

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
	preRunRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failRunWithRedactor(ctx, request, ws, session, ws.BaseCommit, codexAuthenticationFailure, "", nil)
	}
	baseCommit, currentBranch, clean, err := c.Workspace.status(ctx, ws)
	if err != nil {
		return Candidate{}, err
	}
	if currentBranch != ws.Branch {
		return c.failRunWithRedactor(ctx, request, ws, session, baseCommit, "workspace branch changed before the worker started", "", &preRunRedactor)
	}
	if !clean {
		return c.failRunWithRedactor(ctx, request, ws, session, baseCommit, "workspace was not clean before the worker started", "", &preRunRedactor)
	}
	imageDigest, toolVersions, err := c.activeWorkerRuntime(ctx)
	if err != nil {
		return Candidate{}, err
	}
	schemaPath := c.Workspace.schemaPath(ws.CodexState)
	if err := os.WriteFile(schemaPath, []byte(candidateoutput.Schema), 0o600); err != nil {
		return Candidate{}, fmt.Errorf("write Candidate output schema: %w", err)
	}
	command := []string{"codex", "exec", "--sandbox", "danger-full-access", "--json", "--output-schema", schemaPath, "--skip-git-repo-check", request.Prompt}
	if session.CodexSessionID != "" {
		command = []string{"codex", "exec", "--sandbox", "danger-full-access", "resume", "--json", "--output-schema", schemaPath, "--skip-git-repo-check", session.CodexSessionID, request.Prompt}
	}
	environment := map[string]string{
		"CODEX_HOME": ws.CodexState,
	}
	spec := worker.Spec{
		RunID:   request.Claim.RunID,
		Command: command, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch,
		AgentIdentity: session.AgentIdentity, ImageDigest: imageDigest, ToolVersions: toolVersions,
		Environment: environment,
		Mounts:      []worker.Mount{{Source: ws.Path, Target: "/workspace"}, {Source: ws.CodexState, Target: "/codex-state"}},
		ExtraHosts:  []string{worker.GatewayHostMapping},
	}
	if err := spec.Validate(); err != nil {
		return Candidate{}, err
	}
	if err := c.Store.ReserveWorkerLaunch(ctx, request.Claim, c.now()); err != nil {
		return Candidate{}, err
	}
	runCtx := ctx
	cancelRun := func() {}
	if !request.Claim.LeaseExpiresAt.IsZero() {
		runCtx, cancelRun = context.WithDeadline(ctx, request.Claim.LeaseExpiresAt)
	}
	result, runErr := c.Runtime.Run(runCtx, spec)
	defer cancelRun()
	handoffCtx := context.WithoutCancel(ctx)
	output := runtimeOutput(result)
	postRunRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, codexAuthenticationFailure, "", nil)
	}
	runRedactor := preRunRedactor.Merge(postRunRedactor)
	if err := c.Store.RecordWorkerAudit(handoffCtx, store.WorkerAudit{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, ContainerID: result.ContainerID, ImageDigest: spec.ImageDigest, Mounts: spec.Mounts, ExtraHosts: spec.ExtraHosts, ToolVersions: spec.ToolVersions}); err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
	}
	codexOutput := runtimeStdout(result)
	codexSessionID, _ := parseSessionID(codexOutput, session.CodexSessionID)
	if codexSessionID != "" && codexSessionID != session.CodexSessionID {
		if err := c.Store.RecordCodexSession(handoffCtx, request.Claim.RunID, request.Claim.LeaseToken, codexSessionID); err != nil {
			return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
		}
		session.CodexSessionID = codexSessionID
	}
	if runErr != nil || result.ExitCode != 0 {
		return c.failRunWithFailureClass(handoffCtx, request, ws, session, baseCommit, errorText(runErr, result.ExitCode), string(output), &runRedactor, failureClass(runErr))
	}
	commit, currentBranch, clean, err := c.Workspace.status(handoffCtx, ws)
	if err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
	}
	if currentBranch != ws.Branch {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, fmt.Sprintf("worker changed workspace branch to %q", currentBranch), string(output), &runRedactor)
	}
	if !clean {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "worker completed with a dirty workspace", string(output), &runRedactor)
	}
	codexSessionID, structured, err := parseOutput(codexOutput, session.CodexSessionID)
	if err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
	}
	var candidateOutput struct {
		Commit        string               `json:"commit"`
		PlanAmendment *store.PlanAmendment `json:"plan_amendment"`
	}
	if err := json.Unmarshal(structured, &candidateOutput); err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "Codex structured result is invalid JSON", string(output), &runRedactor)
	}
	if candidateOutput.PlanAmendment != nil {
		if commit != baseCommit || candidateOutput.Commit != "" {
			return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "Ticket Agent cannot combine a Plan Amendment with an implementation commit", string(output), &runRedactor)
		}
		candidateOutput.PlanAmendment.VersionID = request.Claim.VersionID
		candidateOutput.PlanAmendment.TicketID = request.Claim.TicketID
		if _, err := c.Store.ProposePlanAmendment(handoffCtx, *candidateOutput.PlanAmendment, c.now()); err != nil {
			return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
		}
		return Candidate{RunID: request.Claim.RunID, SessionID: session.SessionID, CodexSessionID: codexSessionID, StructuredOutput: structured}, nil
	}
	if commit == baseCommit {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "worker produced no new commit", string(output), &runRedactor)
	}
	if candidateOutput.Commit != "" && candidateOutput.Commit != commit {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "Codex structured result names a different commit", string(output), &runRedactor)
	}
	gatewayURL := strings.TrimSpace(c.GatewayURL)
	if gatewayURL == "" {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "Gateway URL is required before candidate acceptance", string(output), &runRedactor)
	}
	publication := request.Publication
	if publication.Body == "" {
		publication.Body = candidateSummary(structured)
	}
	deliveryClaim, err := c.Store.AcceptCandidateForDelivery(handoffCtx, store.CandidateRevision{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, CodexSessionID: codexSessionID, CommitSHA: commit, StructuredOutput: structured, ImageDigest: imageDigest, ToolVersions: toolVersions, Now: c.now(), Publication: publication}, c.deliveryLeaseTTL())
	if err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
	}
	session.AcceptedCommit = commit
	session.AcceptedCandidateRunID = request.Claim.RunID
	candidate := Candidate{RunID: request.Claim.RunID, SessionID: session.SessionID, CodexSessionID: codexSessionID, Commit: commit, StructuredOutput: structured}
	if err := c.runDeliveryController(handoffCtx, deliveryClaim, session, ws, publication, request.Prompt, imageDigest, toolVersions); err != nil {
		return candidate, err
	}
	return candidate, nil
}

func (c Controller) RetryDelivery(ctx context.Context, claim store.TicketClaim) error {
	if c.Store == nil || c.Runtime == nil {
		return errors.New("agent controller dependencies are incomplete")
	}
	if err := c.Store.RequireCurrentDeliveryLease(ctx, claim, c.now()); err != nil {
		return err
	}
	finalizationCtx, cancelFinalization := context.WithDeadline(context.Background(), claim.LeaseExpiresAt.Add(10*time.Second))
	defer cancelFinalization()
	session, err := c.Store.TicketSession(finalizationCtx, claim.VersionID, claim.TicketID)
	if err != nil {
		return c.failDeliveryController(finalizationCtx, claim, err)
	}
	delivery, err := c.Store.CandidateDelivery(finalizationCtx, claim.VersionID, claim.TicketID)
	if err != nil {
		return c.failDeliveryController(finalizationCtx, claim, err)
	}
	if session.WorkspacePath == "" || session.CodexStatePath == "" || session.Branch == "" || session.AcceptedCommit == "" || session.AcceptedCandidateRunID == "" {
		return c.failDeliveryController(finalizationCtx, claim, errors.New("accepted Candidate workspace is incomplete"))
	}
	ws := workspace{Path: session.WorkspacePath, CodexState: session.CodexStatePath, Branch: session.Branch}
	commit, branch, clean, err := c.Workspace.status(finalizationCtx, ws)
	if err != nil {
		return c.failDeliveryController(finalizationCtx, claim, err)
	}
	if branch != ws.Branch || !clean || commit != session.AcceptedCommit {
		return c.failDeliveryController(finalizationCtx, claim, errors.New("accepted Candidate workspace no longer matches its delivery revision"))
	}
	if err := validateLocalRemotes(finalizationCtx, ws.Path); err != nil {
		return c.failDeliveryController(finalizationCtx, claim, err)
	}
	imageDigest, toolVersions, err := c.Store.CandidateWorkerRuntime(finalizationCtx, claim.VersionID, claim.TicketID)
	if err != nil {
		return c.failDeliveryController(finalizationCtx, claim, fmt.Errorf("resolve accepted Candidate runtime: %w", err))
	}
	publication := store.CandidatePublication{
		Repository:         delivery.Repository,
		Branch:             delivery.Branch,
		ExpectedRemoteHead: delivery.RemoteHead,
		ExpectRemoteAbsent: delivery.RemoteHead == "",
		Title:              claim.TicketTitle,
		Body:               "Retry delivery of the accepted Candidate Revision.",
	}
	intent := fmt.Sprintf("Retry delivery of accepted Candidate Revision %s for ticket #%d.", delivery.CandidateCommit, claim.TicketNumber)
	return c.runDeliveryController(finalizationCtx, claim, session, ws, publication, intent, imageDigest, toolVersions)
}

func (c Controller) runDeliveryController(ctx context.Context, deliveryClaim store.TicketClaim, session store.TicketSession, ws workspace, publication store.CandidatePublication, intent, imageDigest string, toolVersions map[string]string) error {
	gatewayURL := strings.TrimSpace(c.GatewayURL)
	if gatewayURL == "" {
		return c.failDeliveryController(ctx, deliveryClaim, errors.New("Gateway URL is required before delivery launch"))
	}
	if session.SessionID == "" || session.AcceptedCandidateRunID == "" {
		return c.failDeliveryController(ctx, deliveryClaim, errors.New("Delivery Cycle or Revision Round is incomplete"))
	}
	noMistakes := c.NoMistakes
	if noMistakes == "" {
		noMistakes = "no-mistakes"
	}
	deliveryEnvironment := map[string]string{
		"CODEX_HOME":                       ws.CodexState,
		"NO_MISTAKES_WORKFLOW_MODE":        "true",
		"NO_MISTAKES_DELIVERY_CYCLE":       session.SessionID,
		"NO_MISTAKES_REVISION_ROUND":       session.AcceptedCandidateRunID,
		"NO_MISTAKES_CORRELATION_ID":       deliveryClaim.RunID,
		"NO_MISTAKES_RUN_ID":               deliveryClaim.RunID,
		"NO_MISTAKES_LEASE_TOKEN":          deliveryClaim.LeaseToken,
		"NO_MISTAKES_LEASE_GENERATION":     fmt.Sprint(deliveryClaim.LeaseGeneration),
		"NO_MISTAKES_REPOSITORY":           publication.Repository,
		"NO_MISTAKES_BRANCH":               publication.Branch,
		"NO_MISTAKES_COMMIT_SHA":           session.AcceptedCommit,
		"NO_MISTAKES_EXPECTED_REMOTE_HEAD": publication.ExpectedRemoteHead,
		"NO_MISTAKES_EXPECT_REMOTE_ABSENT": fmt.Sprint(publication.ExpectRemoteAbsent),
		"NO_MISTAKES_PULL_REQUEST_TITLE":   publication.Title,
		"NO_MISTAKES_PULL_REQUEST_BODY":    publication.Body,
	}
	deliveryEnvironment["NO_MISTAKES_GATEWAY_URL"] = gatewayURL
	if gate, answer, err := c.Store.DeliveryQualityGateAnswer(ctx, session.SessionID); err == nil {
		deliveryEnvironment["NO_MISTAKES_GATE_ID"] = gate.GateID
		deliveryEnvironment["NO_MISTAKES_GATE_FINDING_ID"] = gate.FindingID
		deliveryEnvironment["NO_MISTAKES_GATE_ANSWER"] = answer
		deliveryEnvironment["NO_MISTAKES_GATE_ACTION"] = gate.Action
	} else if !errors.Is(err, store.ErrNotFound) {
		return c.failDeliveryController(ctx, deliveryClaim, fmt.Errorf("load quality gate answer: %w", err))
	}
	deliveryEnvironment["NO_MISTAKES_GATE_ENFORCED"] = "true"
	deliverySpec := worker.Spec{
		RunID:   deliveryClaim.RunID,
		Command: []string{noMistakes, "axi", "run", "--intent", intent}, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch,
		AgentIdentity: session.AgentIdentity, ImageDigest: imageDigest, ToolVersions: toolVersions,
		Environment: deliveryEnvironment,
		Mounts:      []worker.Mount{{Source: ws.Path, Target: "/workspace"}, {Source: ws.CodexState, Target: "/codex-state"}},
		ExtraHosts:  []string{worker.GatewayHostMapping},
	}
	if err := deliverySpec.Validate(); err != nil {
		return c.failDeliveryController(ctx, deliveryClaim, err)
	}
	preDeliveryRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failDeliveryController(ctx, deliveryClaim, errors.New(codexAuthenticationFailure))
	}
	if err := c.Store.ReserveDeliveryControllerLaunch(ctx, deliveryClaim, c.now()); err != nil {
		if errors.Is(err, store.ErrWorkerLaunched) {
			return err
		}
		return c.failDeliveryController(ctx, deliveryClaim, err)
	}
	deliveryCtx, cancelDelivery := context.WithDeadline(context.Background(), deliveryClaim.LeaseExpiresAt)
	defer cancelDelivery()
	deliveryResult, deliveryErr := c.Runtime.Run(deliveryCtx, deliverySpec)
	finalizationCtx, cancelFinalization := context.WithDeadline(context.Background(), deliveryClaim.LeaseExpiresAt.Add(10*time.Second))
	defer cancelFinalization()
	postDeliveryRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failDeliveryController(finalizationCtx, deliveryClaim, errors.New(codexAuthenticationFailure))
	}
	deliveryRedactor := preDeliveryRedactor.Merge(postDeliveryRedactor)
	if outcome, parseErr := parseDeliveryOutcome(runtimeStdout(deliveryResult)); parseErr == nil && outcome.Gate != nil {
		if _, err := c.Store.PauseDeliveryControllerForQualityGate(finalizationCtx, deliveryClaim, *outcome.Gate, c.now()); err != nil {
			return c.failDeliveryController(finalizationCtx, deliveryClaim, err)
		}
		return nil
	} else if deliveryErr != nil || deliveryResult.ExitCode != 0 {
		return c.failDeliveryControllerWithClass(finalizationCtx, deliveryClaim, errors.New(deliveryRedactor.String(errorText(deliveryErr, deliveryResult.ExitCode))), failureClass(deliveryErr))
	} else if parseErr != nil {
		return c.failDeliveryControllerWithClass(finalizationCtx, deliveryClaim, errors.New(deliveryRedactor.String(parseErr.Error())), store.FailureCodeQuality)
	} else if outcome.Gate != nil {
		return c.failDeliveryController(finalizationCtx, deliveryClaim, errors.New("Delivery Controller reported a human gate without pausing"))
	} else if !outcome.Passed {
		return c.failDeliveryController(finalizationCtx, deliveryClaim, errors.New("Delivery Controller did not pass"))
	}
	if err := c.Store.CompleteDeliveryController(finalizationCtx, deliveryClaim, c.now()); err != nil {
		return err
	}
	return nil
}

func (c Controller) activeWorkerRuntime(ctx context.Context) (string, map[string]string, error) {
	activeRelease, err := c.Store.ActiveWorkerRelease(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Active Worker Image: %w", err)
	}
	var releaseManifest struct {
		CodexVersion      string `json:"codex_version"`
		GoVersion         string `json:"go_version"`
		NoMistakesVersion string `json:"no_mistakes_version"`
	}
	if err := json.Unmarshal([]byte(activeRelease.ManifestJSON), &releaseManifest); err != nil ||
		releaseManifest.CodexVersion == "" || releaseManifest.GoVersion == "" || releaseManifest.NoMistakesVersion == "" {
		return "", nil, errors.New("Active Worker Image has an invalid release manifest")
	}
	return activeRelease.ImageReference, map[string]string{"codex": releaseManifest.CodexVersion, "go": releaseManifest.GoVersion, "no-mistakes": releaseManifest.NoMistakesVersion}, nil
}

func (c Controller) failDeliveryController(ctx context.Context, claim store.TicketClaim, cause error) error {
	return errors.Join(cause, c.Store.FailDeliveryController(ctx, claim, cause.Error(), c.now()))
}

func (c Controller) failDeliveryControllerWithClass(ctx context.Context, claim store.TicketClaim, cause error, class store.FailureClass) error {
	return errors.Join(cause, c.Store.FailDeliveryControllerWithClass(ctx, claim, cause.Error(), class, c.now()))
}

func runtimeStdout(result worker.Result) []byte {
	if len(result.Stdout) > 0 {
		return result.Stdout
	}
	return result.Output
}

func runtimeOutput(result worker.Result) []byte {
	if len(result.Output) > 0 {
		return result.Output
	}
	return append(append([]byte(nil), result.Stdout...), result.Stderr...)
}

type deliveryOutcome struct {
	Passed bool
	Gate   *store.QualityGate
}

func parseDeliveryOutcome(output []byte) (deliveryOutcome, error) {
	value, err := toon.Decode(output)
	if err != nil {
		return deliveryOutcome{}, fmt.Errorf("Delivery Controller returned invalid TOON: %w", err)
	}
	document, ok := value.(map[string]any)
	if !ok {
		return deliveryOutcome{}, errors.New("Delivery Controller TOON must be an object")
	}
	run, ok := document["run"].(map[string]any)
	if !ok {
		return deliveryOutcome{}, errors.New("Delivery Controller TOON did not contain a run")
	}
	status, _ := run["status"].(string)
	if gate, err := parseQualityGate(document); err != nil {
		return deliveryOutcome{}, err
	} else if gate != nil {
		return deliveryOutcome{Gate: gate}, nil
	}
	if strings.TrimSpace(status) != "completed" {
		return deliveryOutcome{}, errors.New("Delivery Controller TOON did not complete")
	}
	if outcome, ok := document["outcome"].(string); !ok || (strings.TrimSpace(outcome) != "passed" && strings.TrimSpace(outcome) != "checks-passed") {
		return deliveryOutcome{}, errors.New("Delivery Controller TOON did not pass")
	}
	return deliveryOutcome{Passed: true}, nil
}

func parseQualityGate(document map[string]any) (*store.QualityGate, error) {
	value, found := document["gate"]
	if !found {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("Delivery Controller gate is invalid: %w", err)
	}
	var gate struct {
		ID             string   `json:"id"`
		Source         string   `json:"source"`
		FindingID      string   `json:"finding_id"`
		Action         string   `json:"action"`
		Reason         string   `json:"reason"`
		AllowedAnswers []string `json:"allowed_answers"`
	}
	if err := json.Unmarshal(encoded, &gate); err != nil {
		return nil, fmt.Errorf("Delivery Controller gate is invalid: %w", err)
	}
	if gate.Action != store.QualityGateAskUser && gate.Action != store.QualityGateSkip {
		if gate.Action == "auto-fix" && strings.TrimSpace(gate.FindingID) == "" {
			return nil, errors.New("Delivery Controller auto-fix gate omitted its finding ID")
		}
		if gate.Action == "auto-fix" || gate.Action == "no-op" {
			return nil, nil
		}
		return nil, fmt.Errorf("Delivery Controller gate has unsupported action %q", gate.Action)
	}
	return &store.QualityGate{Source: gate.Source, GateID: gate.ID, FindingID: gate.FindingID, Action: gate.Action, Reason: gate.Reason, AllowedAnswers: gate.AllowedAnswers}, nil
}

func (c Controller) failRun(ctx context.Context, request RunRequest, ws workspace, session store.TicketSession, baseCommit, reason, output string) (Candidate, error) {
	redactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failRunWithRedactor(ctx, request, ws, session, baseCommit, reason, "", nil)
	}
	return c.failRunWithRedactor(ctx, request, ws, session, baseCommit, reason, output, &redactor)
}

func (c Controller) failRunWithRedactor(ctx context.Context, request RunRequest, ws workspace, session store.TicketSession, baseCommit, reason, output string, redactor *codexauth.Redactor) (Candidate, error) {
	return c.failRunWithFailureClass(ctx, request, ws, session, baseCommit, reason, output, redactor, store.FailureCodeQuality)
}

func (c Controller) failRunWithFailureClass(ctx context.Context, request RunRequest, ws workspace, session store.TicketSession, baseCommit, reason, output string, redactor *codexauth.Redactor, class store.FailureClass) (Candidate, error) {
	safeReason := codexAuthenticationFailure
	if redactor != nil {
		safeReason = redactor.String(reason)
	}
	diagnostic, diagnosticErr := c.Workspace.diagnostic(ctx, ws, request.Claim.RunID, baseCommit, output, safeReason, redactor)
	restoreCommit := session.AcceptedCommit
	if restoreCommit == "" {
		restoreCommit = baseCommit
	}
	_, restoreErr := c.Store.WithCurrentAgentLease(ctx, request.Claim, c.now(), func() error {
		return c.Workspace.restore(ctx, ws, restoreCommit)
	})
	var recordErr error
	var cause error
	if redactor == nil {
		cause = &store.SessionAuthenticationFailure{}
	}
	recordErr = c.Store.RecordRunFailure(ctx, store.RunFailure{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, DiagnosticsPath: diagnostic, Error: safeReason, Class: class, Cause: cause, Now: c.now()})
	return Candidate{}, errors.Join(errors.New(safeReason), redactFailureError(diagnosticErr, redactor), redactFailureError(restoreErr, redactor), redactFailureError(recordErr, redactor))
}

func failureClass(err error) store.FailureClass {
	if worker.IsInfrastructureFailure(err) {
		return store.FailureInfrastructure
	}
	return store.FailureCodeQuality
}

func redactFailureError(err error, redactor *codexauth.Redactor) error {
	if err == nil {
		return nil
	}
	if redactor == nil {
		return errors.New("failure detail omitted because Codex authentication could not be safely redacted")
	}
	return errors.New(redactor.String(err.Error()))
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
			if candidateoutput.Validate(candidate) == nil {
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
