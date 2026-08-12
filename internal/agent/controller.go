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
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/isolation"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
	"github.com/skyhuang233/workflow/internal/workerrelease"
	toon "github.com/toon-format/toon-go"
)

type Controller struct {
	Store             *store.Store
	Workspace         WorkspaceManager
	Runtime           worker.Runtime
	ImageDigest       string
	ToolVersions      map[string]string
	NoMistakes        string
	GatewayURL        string
	SourceRepository  string
	DeliveryLeaseTTL  time.Duration
	MaxWorkerAttempts int
	Now               func() time.Time
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

func (c Controller) maxWorkerAttempts() int {
	if c.MaxWorkerAttempts > 0 {
		return c.MaxWorkerAttempts
	}
	return store.DefaultMaxWorkerAttempts
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

const (
	codexAuthenticationFailure           = "Ticket Session Codex authentication cache is unavailable"
	workerAuditTimeout                   = 10 * time.Second
	deliverySourceIntegrityExitCode      = 78
	deliverySourceInfrastructureExitCode = 79
	deliverySourceContainerPreflight     = `
integrity_failure() {
  echo "Delivery Source failed isolated launch revalidation" >&2
  exit 78
}
infrastructure_failure() {
  echo "Delivery Source isolated launch preparation failed" >&2
  exit 79
}
git --git-dir=/source-seed fsck --connectivity-only --no-dangling >/dev/null 2>&1 || integrity_failure
if actual=$(delivery-source-digest /source-seed); then
  :
else
  status=$?
  case "$status" in
    126|127) infrastructure_failure ;;
    *) integrity_failure ;;
  esac
fi
identity=$(git --git-dir=/source-seed config --local --get workflow.sourceIdentity) || integrity_failure
[ "$actual" = "$NO_MISTAKES_DELIVERY_SOURCE_DIGEST" ] || integrity_failure
rm -rf /source-repository || infrastructure_failure
git clone --bare --no-local /source-seed /source-repository >/dev/null 2>&1 || infrastructure_failure
git --git-dir=/source-repository remote remove origin || infrastructure_failure
git --git-dir=/source-repository config --local workflow.sourceIdentity "$identity" || infrastructure_failure
if "$@"; then
  exit 0
else
  status=$?
  [ "$status" -eq 78 ] && exit 80
  [ "$status" -eq 79 ] && exit 81
  exit "$status"
fi
`
)

func (c Controller) Run(ctx context.Context, request RunRequest) (Candidate, error) {
	if c.Store == nil || c.Runtime == nil {
		return Candidate{}, errors.New("agent controller dependencies are incomplete")
	}
	if request.Claim.SessionID == "" || request.Claim.RunID == "" || request.Prompt == "" {
		return Candidate{}, store.ErrInvalidClaim
	}
	claim, session, err := c.Store.ResolveAgentLaunchContext(ctx, request.Claim, c.now())
	if err != nil {
		return Candidate{}, err
	}
	request.Claim = claim
	revisionRoundID, err := c.Store.RevisionRoundID(ctx, request.Claim.RunID)
	if err != nil {
		return Candidate{}, err
	}
	ws, err := c.Workspace.ensure(ctx, session.SessionID, revisionRoundID, request.SourceRepository, request.Branch)
	if err != nil {
		return c.failInitialSource(context.WithoutCancel(ctx), request, err)
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
		RunID: request.Claim.RunID, RunKind: store.RunAgent,
		Command: command, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch,
		AgentIdentity: session.AgentIdentity, ImageDigest: imageDigest, ToolVersions: toolVersions,
		Environment: environment,
		Mounts:      []worker.Mount{{Source: ws.Path, Target: "/workspace"}, {Source: ws.CodexState, Target: "/codex-state"}},
		ExtraHosts:  []string{worker.GatewayHostMapping},
	}
	spec.ContainerCreateFence = func(createCtx context.Context) (func(context.Context) error, error) {
		return c.Store.AcquireWorkerContainerCreateFence(createCtx, request.Claim, c.now())
	}
	spec.StartAdmission = func(startCtx context.Context) error {
		return c.Store.ReserveWorkerLaunch(startCtx, request.Claim, workerLaunchAudit(request.Claim, spec), c.now())
	}
	if err := spec.Validate(); err != nil {
		return Candidate{}, err
	}
	if err := c.Store.ReserveWorkerPrelaunch(ctx, request.Claim, c.now()); err != nil {
		return Candidate{}, err
	}
	runCtx := ctx
	cancelRun := func() {}
	if !request.Claim.LeaseExpiresAt.IsZero() {
		runCtx, cancelRun = context.WithDeadline(ctx, request.Claim.LeaseExpiresAt)
	}
	result, runErr := c.Runtime.Run(runCtx, spec)
	defer cancelRun()
	if runErr != nil && (worker.IsUncertainContainerStateFailure(runErr) || worker.IsPreparedContainerCleanupFailure(runErr)) {
		isolationCtx := context.WithoutCancel(ctx)
		target, targetErr := c.Store.WorkerContainerIsolationTarget(isolationCtx, request.Claim)
		if targetErr != nil {
			return Candidate{}, errors.Join(runErr, targetErr)
		}
		if _, isolateErr := c.isolateWorkerTargets(isolationCtx, []store.TicketClaim{target}); isolateErr != nil {
			return Candidate{}, errors.Join(runErr, isolateErr)
		}
	}
	auditErr := c.recordWorkerContainer(request.Claim, result)
	handoffCtx := context.WithoutCancel(ctx)
	if auditErr != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, preRunRedactor.String(auditErr.Error()), "", nil)
	}
	output := runtimeOutput(result)
	postRunRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, codexAuthenticationFailure, "", nil)
	}
	runRedactor := preRunRedactor.Merge(postRunRedactor)
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
		if _, err := c.proposePlanAmendment(handoffCtx, *candidateOutput.PlanAmendment); err != nil {
			return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
		}
		return Candidate{RunID: request.Claim.RunID, SessionID: session.SessionID, CodexSessionID: codexSessionID, StructuredOutput: structured}, nil
	}
	if commit == baseCommit {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "worker produced no new commit", string(output), &runRedactor)
	}
	if candidateOutput.Commit != commit {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "Codex structured result must name the workspace HEAD commit", string(output), &runRedactor)
	}
	gatewayURL := strings.TrimSpace(c.GatewayURL)
	if gatewayURL == "" {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, "Gateway URL is required before candidate acceptance", string(output), &runRedactor)
	}
	publication := request.Publication
	if publication.Body == "" {
		publication.Body = candidateSummary(structured)
	}
	deliveryClaim, err := c.Store.AcceptCandidateForDelivery(handoffCtx, store.CandidateRevision{RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken, CodexSessionID: codexSessionID, CommitSHA: commit, StructuredOutput: structured, ImageDigest: imageDigest, ToolVersions: toolVersions, DeliverySourceDigest: ws.DeliverySourceDigest, Now: c.now(), Publication: publication}, c.deliveryLeaseTTL())
	if err != nil {
		return c.failRunWithRedactor(handoffCtx, request, ws, session, baseCommit, err.Error(), string(output), &runRedactor)
	}
	session.AcceptedCommit = commit
	session.AcceptedCandidateRunID = request.Claim.RunID
	candidate := Candidate{RunID: request.Claim.RunID, SessionID: session.SessionID, CodexSessionID: codexSessionID, Commit: commit, StructuredOutput: structured}
	if err := c.Store.ReserveDeliveryControllerPrelaunch(handoffCtx, deliveryClaim, c.now()); err != nil {
		return candidate, err
	}
	if err := c.runDeliveryController(handoffCtx, deliveryClaim, session, ws, publication, request.Prompt); err != nil {
		return candidate, err
	}
	return candidate, nil
}

func (c Controller) RetryDelivery(ctx context.Context, claim store.TicketClaim) error {
	if c.Store == nil || c.Runtime == nil {
		return errors.New("agent controller dependencies are incomplete")
	}
	claim, session, delivery, err := c.Store.ResolveDeliveryLaunchContext(ctx, claim, c.now())
	if err != nil {
		return err
	}
	if err := c.Store.ReserveDeliveryControllerPrelaunch(ctx, claim, c.now()); err != nil {
		return err
	}
	finalizationCtx, cancelFinalization := context.WithDeadline(context.Background(), claim.LeaseExpiresAt.Add(10*time.Second))
	defer cancelFinalization()
	expectedSourceDigest, err := c.Store.CandidateDeliverySourceDigest(finalizationCtx, session.AcceptedCandidateRunID)
	if err != nil {
		return c.failDeliverySourcePreflight(finalizationCtx, claim, deliverySourceInfrastructureError(fmt.Errorf("load Candidate Delivery Source provenance: %w", err)))
	}
	expectedSourceDigest = strings.TrimSpace(expectedSourceDigest)
	if session.WorkspacePath == "" || session.CodexStatePath == "" || session.Branch == "" || session.AcceptedCommit == "" || session.AcceptedCandidateRunID == "" {
		return c.failDeliveryController(finalizationCtx, claim, errors.New("accepted Candidate workspace is incomplete"))
	}
	revisionRoundID, err := c.Store.RevisionRoundID(finalizationCtx, session.AcceptedCandidateRunID)
	if err != nil {
		return c.failDeliverySourcePreflight(finalizationCtx, claim, deliverySourceInfrastructureError(fmt.Errorf("load accepted Revision Round: %w", err)))
	}
	deliverySource, err := c.Workspace.deliverySourcePath(session.SessionID, revisionRoundID)
	if err != nil {
		return c.failDeliverySourcePreflight(finalizationCtx, claim, err)
	}
	_, statErr := os.Stat(deliverySource)
	sourceWasMissing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !sourceWasMissing {
		return c.failDeliverySourcePreflight(finalizationCtx, claim, deliverySourceInfrastructureError(fmt.Errorf("inspect persisted Delivery Source: %w", statErr)))
	}
	sourceRepository, err := admittedSourceRepository(finalizationCtx, session.WorkspacePath, c.SourceRepository)
	if err != nil {
		return c.failDeliverySourcePreflight(finalizationCtx, claim, err)
	}
	if sourceWasMissing {
		sourceRepository, err = localSourceRepository(sourceRepository)
		if err != nil {
			return c.failDeliverySourcePreflight(finalizationCtx, claim, err)
		}
		if err := replaceWorkspaceOriginURLs(finalizationCtx, session.WorkspacePath, []string{sourceRepository}); err != nil {
			return c.failDeliverySourcePreflight(finalizationCtx, claim, deliverySourceInfrastructureError(fmt.Errorf("restore ticket workspace origin: %w", err)))
		}
		deliverySource, err = c.Workspace.ensureDeliverySource(finalizationCtx, session.SessionID, revisionRoundID, sourceRepository)
	} else {
		err = validateDeliverySource(finalizationCtx, deliverySource)
	}
	if err != nil {
		return c.failDeliverySourcePreflight(finalizationCtx, claim, err)
	}
	if expectedSourceDigest == "" {
		expectedSourceDigest, err = digestDeliverySource(finalizationCtx, deliverySource)
		if err != nil {
			return c.failDeliverySourcePreflight(finalizationCtx, claim, err)
		}
		if err := c.Store.BackfillLegacyCandidateDeliverySourceDigest(finalizationCtx, claim, session.AcceptedCandidateRunID, expectedSourceDigest, c.now()); err != nil {
			return c.failDeliverySourcePreflight(finalizationCtx, claim, deliverySourceInfrastructureError(fmt.Errorf("pin legacy Candidate Delivery Source provenance: %w", err)))
		}
	} else if sourceWasMissing {
		if err := verifyDeliverySourceDigest(finalizationCtx, deliverySource, expectedSourceDigest); err != nil {
			removeErr := os.RemoveAll(deliverySource)
			return c.failDeliverySourcePreflight(finalizationCtx, claim, errors.Join(err, removeErr))
		}
	}
	ws := workspace{Path: session.WorkspacePath, CodexState: session.CodexStatePath, DeliverySource: deliverySource, RevisionRoundID: revisionRoundID, SourceRepository: sourceRepository, Branch: session.Branch}
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
	publication := store.CandidatePublication{
		Repository:         delivery.Repository,
		Branch:             delivery.Branch,
		ExpectedRemoteHead: delivery.RemoteHead,
		ExpectRemoteAbsent: delivery.RemoteHead == "",
		Title:              claim.TicketTitle,
		Body:               "Retry delivery of the accepted Candidate Revision.",
	}
	intent := fmt.Sprintf("Retry delivery of accepted Candidate Revision %s for ticket #%d.", delivery.CandidateCommit, claim.TicketNumber)
	return c.runDeliveryController(finalizationCtx, claim, session, ws, publication, intent)
}

func (c Controller) runDeliveryController(ctx context.Context, deliveryClaim store.TicketClaim, session store.TicketSession, ws workspace, publication store.CandidatePublication, intent string) (resultErr error) {
	gatewayURL := strings.TrimSpace(c.GatewayURL)
	if gatewayURL == "" {
		return c.failDeliveryController(ctx, deliveryClaim, errors.New("Gateway URL is required before delivery launch"))
	}
	if session.SessionID == "" || session.AcceptedCandidateRunID == "" || ws.RevisionRoundID == "" {
		return c.failDeliveryController(ctx, deliveryClaim, errors.New("Delivery Cycle or Revision Round is incomplete"))
	}
	noMistakes := c.NoMistakes
	if noMistakes == "" {
		noMistakes = "no-mistakes"
	}
	defaultBranch, expectedSourceDigest, err := c.validateDeliverySourceForLaunch(ctx, session, ws.DeliverySource)
	if err != nil {
		return c.failDeliverySourcePreflight(ctx, deliveryClaim, err)
	}
	imageDigest, toolVersions, err := c.deliveryControllerRuntime(ctx, deliveryClaim)
	if err != nil {
		return c.failDeliveryController(ctx, deliveryClaim, err)
	}
	deliveryEnvironment := map[string]string{
		"CODEX_HOME":                         ws.CodexState,
		"GIT_CONFIG_COUNT":                   "0",
		"GIT_CONFIG_GLOBAL":                  "/dev/null",
		"GIT_CONFIG_NOSYSTEM":                "1",
		"NM_HOME":                            "/codex-state/no-mistakes",
		"NO_MISTAKES_WORKFLOW_MODE":          "true",
		"NO_MISTAKES_DELIVERY_CYCLE":         session.SessionID,
		"NO_MISTAKES_REVISION_ROUND":         ws.RevisionRoundID,
		"NO_MISTAKES_DELIVERY_SOURCE_DIGEST": expectedSourceDigest,
		"NO_MISTAKES_CORRELATION_ID":         deliveryClaim.RunID,
		"NO_MISTAKES_RUN_ID":                 deliveryClaim.RunID,
		"NO_MISTAKES_LEASE_TOKEN":            deliveryClaim.LeaseToken,
		"NO_MISTAKES_LEASE_GENERATION":       fmt.Sprint(deliveryClaim.LeaseGeneration),
		"NO_MISTAKES_REPOSITORY":             publication.Repository,
		"NO_MISTAKES_DEFAULT_BRANCH":         defaultBranch,
		"NO_MISTAKES_BRANCH":                 publication.Branch,
		"NO_MISTAKES_COMMIT_SHA":             session.AcceptedCommit,
		"NO_MISTAKES_EXPECTED_REMOTE_HEAD":   publication.ExpectedRemoteHead,
		"NO_MISTAKES_EXPECT_REMOTE_ABSENT":   fmt.Sprint(publication.ExpectRemoteAbsent),
		"NO_MISTAKES_PULL_REQUEST_TITLE":     publication.Title,
		"NO_MISTAKES_PULL_REQUEST_BODY":      publication.Body,
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
	preDeliveryRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failDeliveryController(ctx, deliveryClaim, errors.New(codexAuthenticationFailure))
	}
	sealedSource, err := c.Workspace.sealedDeliverySourcePath(session.SessionID, ws.RevisionRoundID, deliveryClaim.RunID, ws.DeliverySource)
	if err != nil {
		return c.failDeliverySourcePreflight(ctx, deliveryClaim, err)
	}
	deliverySpec := worker.Spec{
		RunID: deliveryClaim.RunID, RunKind: store.RunDelivery,
		Command: []string{noMistakes, "axi", "run", "--intent", intent}, WorkspacePath: ws.Path, CodexStatePath: ws.CodexState, Branch: ws.Branch,
		AgentIdentity: session.AgentIdentity, ImageDigest: imageDigest, ToolVersions: toolVersions,
		Environment: deliveryEnvironment,
		Mounts: []worker.Mount{
			{Source: ws.Path, Target: "/workspace"},
			{Source: ws.CodexState, Target: "/codex-state"},
			{Source: sealedSource, Target: "/source-seed", ReadOnly: true},
		},
		ExtraHosts:         []string{worker.GatewayHostMapping},
		ContainerPreflight: deliverySourceContainerPreflight,
	}
	deliverySpec.ContainerCreateFence = func(createCtx context.Context) (func(context.Context) error, error) {
		return c.Store.AcquireDeliveryControllerCreateFence(createCtx, deliveryClaim, c.now())
	}
	deliverySpec.StartAdmission = func(startCtx context.Context) error {
		return c.Store.ReserveDeliveryControllerLaunch(startCtx, deliveryClaim, workerLaunchAudit(deliveryClaim, deliverySpec), c.now())
	}
	if err := deliverySpec.Validate(); err != nil {
		return c.failDeliveryController(ctx, deliveryClaim, err)
	}
	if err := c.Workspace.reclaimSupersededDeliverySources(ctx, session.SessionID, ws.RevisionRoundID); err != nil {
		return c.failDeliveryControllerWithClass(ctx, deliveryClaim, fmt.Errorf("reclaim superseded Delivery Sources: %w", err), store.FailureInfrastructure)
	}
	sealedSource, cleanupSealedSource, err := c.Workspace.sealDeliverySource(ctx, session.SessionID, ws.RevisionRoundID, deliveryClaim.RunID, ws.DeliverySource, expectedSourceDigest)
	if err != nil {
		if cleanupSealedSource != nil {
			err = errors.Join(err, cleanupSealedSource())
		}
		return c.failDeliverySourcePreflight(ctx, deliveryClaim, err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, cleanupSealedSource())
	}()
	deliveryCtx, cancelDelivery := context.WithDeadline(context.Background(), deliveryClaim.LeaseExpiresAt)
	defer cancelDelivery()
	deliveryResult, deliveryErr := runInValidatedDeliveryWorkspace(deliveryCtx, c.Runtime, deliverySpec, ws.SourceRepository, sealedSource, expectedSourceDigest)
	if deliveryErr != nil && worker.IsPreparedContainerCleanupFailure(deliveryErr) {
		target, targetErr := c.Store.DeliveryContainerIsolationTarget(ctx, deliveryClaim.VersionID, deliveryClaim.TicketID)
		if targetErr != nil {
			return errors.Join(deliveryErr, targetErr)
		}
		isolated, isolateErr := c.isolateWorkerTargets(ctx, []store.TicketClaim{target})
		if isolateErr != nil {
			return errors.Join(deliveryErr, isolateErr)
		}
		return errors.Join(deliveryErr, c.Store.FailDeliveryControllerLaunchWithClassAfterIsolation(context.WithoutCancel(ctx), deliveryClaim, deliveryErr.Error(), failureClass(deliveryErr), c.now(), c.maxWorkerAttempts(), isolated...))
	}
	if deliveryErr != nil && worker.IsUncertainContainerStateFailure(deliveryErr) {
		target, targetErr := c.Store.DeliveryContainerIsolationTarget(ctx, deliveryClaim.VersionID, deliveryClaim.TicketID)
		if targetErr != nil {
			return errors.Join(deliveryErr, targetErr)
		}
		isolated, isolateErr := c.isolateWorkerTargets(ctx, []store.TicketClaim{target})
		if isolateErr != nil {
			return errors.Join(deliveryErr, isolateErr)
		}
		return errors.Join(deliveryErr, c.Store.FailDeliveryControllerWithClassAfterIsolation(context.WithoutCancel(ctx), deliveryClaim, deliveryErr.Error(), failureClass(deliveryErr), c.now(), c.maxWorkerAttempts(), isolated...))
	}
	if deliveryErr != nil && worker.IsCertifiedNoLaunchFailure(deliveryErr) {
		return c.failDeliveryControllerLaunchWithClass(context.WithoutCancel(ctx), deliveryClaim, deliveryErr, failureClass(deliveryErr))
	}
	if deliveryErr != nil && deliveryResult.ContainerID == "" {
		var integrityFailure *deliverySourceIntegrityFailure
		var infrastructureFailure *deliverySourceInfrastructureFailure
		if errors.As(deliveryErr, &integrityFailure) || errors.As(deliveryErr, &infrastructureFailure) {
			return c.failDeliverySourcePreflight(context.WithoutCancel(ctx), deliveryClaim, deliveryErr)
		}
		return c.failDeliveryController(context.WithoutCancel(ctx), deliveryClaim, fmt.Errorf("Delivery Controller launch outcome is uncertain: %w", deliveryErr))
	}
	auditErr := c.recordWorkerContainer(deliveryClaim, deliveryResult)
	finalizationCtx, cancelFinalization := context.WithDeadline(context.Background(), deliveryClaim.LeaseExpiresAt.Add(10*time.Second))
	defer cancelFinalization()
	if auditErr != nil {
		return c.failDeliveryController(finalizationCtx, deliveryClaim, errors.New(preDeliveryRedactor.String(auditErr.Error())))
	}
	if deliveryResult.ExitCode == deliverySourceIntegrityExitCode {
		return c.failDeliverySourcePreflight(finalizationCtx, deliveryClaim, deliverySourceIntegrityError(errors.New("mounted Delivery Source failed isolated launch revalidation")))
	}
	if deliveryResult.ExitCode == deliverySourceInfrastructureExitCode {
		return c.failDeliverySourcePreflight(finalizationCtx, deliveryClaim, deliverySourceInfrastructureError(errors.New("mounted Delivery Source could not be prepared for isolated launch")))
	}
	postDeliveryRedactor, err := c.Workspace.authenticationRedactor(ws)
	if err != nil {
		return c.failDeliveryController(finalizationCtx, deliveryClaim, errors.New(codexAuthenticationFailure))
	}
	deliveryRedactor := preDeliveryRedactor.Merge(postDeliveryRedactor)
	outcome, parseErr := parseDeliveryOutcome(runtimeStdout(deliveryResult))
	if deliveryErr != nil || deliveryResult.ExitCode != 0 {
		return c.failDeliveryControllerWithClass(finalizationCtx, deliveryClaim, errors.New(deliveryRedactor.String(errorText(deliveryErr, deliveryResult.ExitCode))), failureClass(deliveryErr))
	} else if parseErr != nil {
		return c.failDeliveryControllerWithClass(finalizationCtx, deliveryClaim, errors.New(deliveryRedactor.String(parseErr.Error())), store.FailureCodeQuality)
	} else if outcome.Gate != nil {
		if _, err := c.Store.PauseDeliveryControllerForQualityGate(finalizationCtx, deliveryClaim, *outcome.Gate, c.now()); err != nil {
			return c.failDeliveryController(finalizationCtx, deliveryClaim, err)
		}
		return nil
	} else if !outcome.Passed {
		return c.failDeliveryController(finalizationCtx, deliveryClaim, errors.New("Delivery Controller did not pass"))
	}
	if err := c.Store.CompleteDeliveryController(finalizationCtx, deliveryClaim, c.now()); err != nil {
		return err
	}
	return nil
}

func runInDeliveryWorkspace(ctx context.Context, runtime worker.Runtime, spec worker.Spec, sourceRepository string) (worker.Result, error) {
	return runInValidatedDeliveryWorkspace(ctx, runtime, spec, sourceRepository, "", "")
}

func runInValidatedDeliveryWorkspace(ctx context.Context, runtime worker.Runtime, spec worker.Spec, sourceRepository, sealedSource, expectedSourceDigest string) (result worker.Result, resultErr error) {
	restore, err := prepareDeliveryWorkspace(ctx, spec.WorkspacePath, sourceRepository)
	if err != nil {
		if ctx.Err() != nil {
			return worker.Result{}, worker.CertifiedNoLaunchError{Err: errors.Join(ctx.Err(), err)}
		}
		var integrityFailure *deliverySourceIntegrityFailure
		var infrastructureFailure *deliverySourceInfrastructureFailure
		if errors.As(err, &integrityFailure) || errors.As(err, &infrastructureFailure) {
			return worker.Result{}, err
		}
		return worker.Result{}, worker.InfrastructureError{Err: err}
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), workerAuditTimeout)
		defer cancel()
		if err := restore(restoreCtx); err != nil {
			resultErr = errors.Join(resultErr, worker.InfrastructureError{Err: err})
		}
	}()
	if sealedSource != "" || expectedSourceDigest != "" {
		if err := verifyDeliverySourceDigest(ctx, sealedSource, expectedSourceDigest); err != nil {
			if ctx.Err() != nil {
				return worker.Result{}, worker.CertifiedNoLaunchError{Err: errors.Join(ctx.Err(), err)}
			}
			return worker.Result{}, err
		}
	}
	result, resultErr = runtime.Run(ctx, spec)
	return result, resultErr
}

func prepareDeliveryWorkspace(ctx context.Context, workspacePath, sourceRepository string) (func(context.Context) error, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return nil, errors.New("Ticket Workspace path is required")
	}
	if err := validateDeliveryWorkspaceTransport(ctx, workspacePath); err != nil {
		return nil, err
	}
	original, err := gitOutput(ctx, workspacePath, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		return nil, fmt.Errorf("read Ticket Workspace origin: %w", err)
	}
	restoreURLs := strings.Split(strings.TrimSpace(original), "\n")
	if strings.TrimSpace(sourceRepository) != "" {
		restoreURLs = []string{sourceRepository}
	}
	if err := replaceWorkspaceOriginURLs(ctx, workspacePath, []string{"/source-repository"}); err != nil {
		return nil, fmt.Errorf("configure Delivery Controller origin: %w", err)
	}
	restore := func(restoreCtx context.Context) error {
		if err := replaceWorkspaceOriginURLs(restoreCtx, workspacePath, restoreURLs); err != nil {
			return fmt.Errorf("restore Ticket Workspace origin: %w", err)
		}
		return nil
	}
	effective, err := trustedGitOutput(ctx, workspacePath, "remote", "get-url", "--all", "origin")
	if err != nil {
		return nil, errors.Join(deliverySourceInfrastructureError(fmt.Errorf("resolve effective Delivery Controller origin: %w", err)), restore(context.WithoutCancel(ctx)))
	}
	if strings.TrimSpace(effective) != "/source-repository" {
		return nil, errors.Join(deliverySourceIntegrityError(errors.New("Ticket Workspace transport configuration rewrites the pinned Delivery Source")), restore(context.WithoutCancel(ctx)))
	}
	return restore, nil
}

func validateDeliveryWorkspaceTransport(ctx context.Context, workspacePath string) error {
	output, err := gitOutput(ctx, workspacePath, "config", "--local", "--includes", "--name-only", "--list", "-z")
	if err != nil {
		return deliverySourceInfrastructureError(fmt.Errorf("inspect Ticket Workspace transport configuration: %w", err))
	}
	for _, key := range strings.Split(output, "\x00") {
		key = strings.ToLower(strings.TrimSpace(key))
		parts := strings.Split(key, ".")
		unsafeRemote := len(parts) >= 3 && parts[0] == "remote" && (parts[len(parts)-1] == "uploadpack" || parts[len(parts)-1] == "receivepack" || parts[len(parts)-1] == "proxy" || parts[len(parts)-1] == "vcs")
		unsafeRewrite := strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof"))
		unsafeInclude := strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.")
		unsafeCore := key == "core.sshcommand" || key == "core.gitproxy"
		unsafeProtocol := strings.HasPrefix(key, "protocol.") && strings.HasSuffix(key, ".allow")
		if unsafeRemote || unsafeRewrite || unsafeInclude || unsafeCore || unsafeProtocol {
			return deliverySourceIntegrityError(fmt.Errorf("Ticket Workspace contains unsafe Git transport configuration %q", key))
		}
	}
	return nil
}

func trustedSourceDefaultBranch(ctx context.Context, sourcePath string) (string, error) {
	_, branch, err := deliverySourceDefaultBranch(ctx, sourcePath)
	return branch, err
}

var (
	errDeliverySourceProvenanceUnavailable = errors.New("Delivery Source provenance is unavailable")
	errDeliverySourceDigestMismatch        = errors.New("Delivery Source does not match its pinned Candidate Revision")
)

func (c Controller) candidateDeliverySourceDigest(ctx context.Context, candidateRunID string) (string, error) {
	expected, err := c.Store.CandidateDeliverySourceDigest(ctx, candidateRunID)
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("load Candidate Delivery Source provenance: %w", err))
	}
	if strings.TrimSpace(expected) == "" {
		return "", deliverySourceIntegrityError(errDeliverySourceProvenanceUnavailable)
	}
	return strings.TrimSpace(expected), nil
}

func (c Controller) validateDeliverySourceForLaunch(ctx context.Context, session store.TicketSession, sourcePath string) (string, string, error) {
	expected, err := c.candidateDeliverySourceDigest(ctx, session.AcceptedCandidateRunID)
	if err != nil {
		return "", "", err
	}
	if err := validateDeliverySource(ctx, sourcePath); err != nil {
		return "", "", err
	}
	if err := verifyDeliverySourceDigest(ctx, sourcePath, expected); err != nil {
		return "", "", err
	}
	branch, err := trustedSourceDefaultBranch(ctx, sourcePath)
	if err != nil {
		return "", "", fmt.Errorf("resolve trusted default branch: %w", err)
	}
	return branch, expected, nil
}

func verifyDeliverySourceDigest(ctx context.Context, sourcePath, expected string) error {
	if strings.TrimSpace(expected) == "" {
		return deliverySourceIntegrityError(errDeliverySourceProvenanceUnavailable)
	}
	actual, err := digestDeliverySource(ctx, sourcePath)
	if err != nil {
		return err
	}
	if actual != strings.TrimSpace(expected) {
		return deliverySourceIntegrityError(errDeliverySourceDigestMismatch)
	}
	return nil
}

func (c Controller) activeWorkerRuntime(ctx context.Context) (string, map[string]string, error) {
	activeRelease, err := c.Store.ActiveWorkerRelease(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Active Worker Image: %w", err)
	}
	provenance, err := workerrelease.DecodeToolProvenance([]byte(activeRelease.ManifestJSON))
	if err != nil {
		return "", nil, errors.New("Active Worker Image has an invalid release manifest")
	}
	toolVersions, _ := provenance.ToolVersions()
	return activeRelease.ImageReference, toolVersions, nil
}

func (c Controller) deliveryControllerRuntime(ctx context.Context, claim store.TicketClaim) (string, map[string]string, error) {
	imageDigest, toolVersions, pinned, err := c.Store.DeliveryWorkerRuntime(ctx, claim)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Delivery Controller runtime: %w", err)
	}
	if pinned {
		return imageDigest, toolVersions, nil
	}
	imageDigest, toolVersions, err = c.activeWorkerRuntime(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Active Worker runtime for delivery recovery: %w", err)
	}
	return imageDigest, toolVersions, nil
}

func workerLaunchAudit(claim store.TicketClaim, spec worker.Spec) store.WorkerAudit {
	return store.WorkerAudit{
		RunID: claim.RunID, LeaseGeneration: claim.LeaseGeneration,
		ImageDigest: spec.ImageDigest, Mounts: spec.Mounts, ExtraHosts: spec.ExtraHosts, ToolVersions: spec.ToolVersions,
	}
}

func (c Controller) recordWorkerContainer(claim store.TicketClaim, result worker.Result) error {
	if strings.TrimSpace(result.ContainerID) == "" {
		return nil
	}
	auditCtx, cancelAudit := context.WithTimeout(context.Background(), workerAuditTimeout)
	defer cancelAudit()
	return c.Store.RecordWorkerContainer(auditCtx, claim.RunID, claim.LeaseGeneration, result.ContainerID)
}

func (c Controller) retryWorkerTransition(ctx context.Context, transition func([]store.WorkerIsolationProof) error) error {
	isolator, _ := c.Runtime.(worker.ContainerIsolator)
	return isolation.RetryWorkerTransition(ctx, c.Store, isolator, transition)
}

func (c Controller) proposePlanAmendment(ctx context.Context, amendment store.PlanAmendment) (store.PlanAmendmentProposal, error) {
	var proposal store.PlanAmendmentProposal
	err := c.retryWorkerTransition(ctx, func(isolated []store.WorkerIsolationProof) error {
		var err error
		proposal, err = c.Store.ProposePlanAmendment(ctx, amendment, c.now(), isolated...)
		return err
	})
	return proposal, err
}

func (c Controller) failDeliveryController(ctx context.Context, claim store.TicketClaim, cause error) error {
	storeErr := c.retryWorkerTransition(ctx, func(isolated []store.WorkerIsolationProof) error {
		return c.Store.FailDeliveryController(ctx, claim, cause.Error(), c.now(), isolated...)
	})
	return errors.Join(cause, storeErr)
}

func (c Controller) failDeliveryControllerWithClass(ctx context.Context, claim store.TicketClaim, cause error, class store.FailureClass) error {
	storeErr := c.retryWorkerTransition(ctx, func(isolated []store.WorkerIsolationProof) error {
		if len(isolated) == 0 {
			return c.Store.FailDeliveryControllerWithClass(ctx, claim, cause.Error(), class, c.now(), c.maxWorkerAttempts())
		}
		return c.Store.FailDeliveryControllerWithClassAfterIsolation(ctx, claim, cause.Error(), class, c.now(), c.maxWorkerAttempts(), isolated...)
	})
	return errors.Join(cause, storeErr)
}

func (c Controller) failDeliveryControllerLaunchWithClass(ctx context.Context, claim store.TicketClaim, cause error, class store.FailureClass) error {
	storeErr := c.retryWorkerTransition(ctx, func(isolated []store.WorkerIsolationProof) error {
		if len(isolated) == 0 {
			return c.Store.FailDeliveryControllerLaunchWithClass(ctx, claim, cause.Error(), class, c.now(), c.maxWorkerAttempts())
		}
		return c.Store.FailDeliveryControllerLaunchWithClassAfterIsolation(ctx, claim, cause.Error(), class, c.now(), c.maxWorkerAttempts(), isolated...)
	})
	return errors.Join(cause, storeErr)
}

func (c Controller) isolateWorkerTargets(ctx context.Context, targets []store.TicketClaim) ([]store.WorkerIsolationProof, error) {
	isolator, ok := c.Runtime.(worker.ContainerIsolator)
	if !ok {
		return nil, errors.New("agent controller cannot isolate an active Worker")
	}
	return isolation.IsolateWorkers(ctx, c.Store, isolator, targets)
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
	if gate.Action == "ask_user" {
		gate.Action = store.QualityGateAskUser
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

func (c Controller) failInitialSource(ctx context.Context, request RunRequest, sourceErr error) (Candidate, error) {
	class := store.FailureCodeQuality
	var infrastructureFailure *deliverySourceInfrastructureFailure
	if errors.As(sourceErr, &infrastructureFailure) || isDeliverySourceAuthenticationFailure(sourceErr) {
		class = store.FailureInfrastructure
	}
	recordErr := c.Store.RecordRunFailure(ctx, store.RunFailure{
		RunID: request.Claim.RunID, LeaseToken: request.Claim.LeaseToken,
		Error: sourceErr.Error(), Class: class, Now: c.now(),
	})
	return Candidate{}, errors.Join(sourceErr, recordErr)
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

func isDeliverySourceAuthenticationFailure(err error) bool {
	if errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		return true
	}
	var authenticationFailure interface{ AuthenticationFailure() bool }
	return errors.As(err, &authenticationFailure) && authenticationFailure.AuthenticationFailure()
}

func (c Controller) failDeliverySourcePreflight(ctx context.Context, claim store.TicketClaim, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		cause := worker.CertifiedNoLaunchError{Err: err}
		return c.failDeliveryControllerLaunchWithClass(context.WithoutCancel(ctx), claim, cause, store.FailureInfrastructure)
	}
	if isDeliverySourceAuthenticationFailure(err) {
		deferErr := c.retryWorkerTransition(ctx, func(isolated []store.WorkerIsolationProof) error {
			return c.Store.DeferDeliveryControllerForCredentialPause(ctx, claim, c.now(), isolated...)
		})
		return errors.Join(err, deferErr)
	}
	var integrityFailure *deliverySourceIntegrityFailure
	if errors.As(err, &integrityFailure) {
		reason := "The accepted Candidate Revision's Delivery Source failed integrity revalidation. Create a new Candidate Revision against a freshly pinned Delivery Source and rerun the complete quality chain."
		if errors.Is(err, errDeliverySourceProvenanceUnavailable) {
			reason = "The accepted Candidate Revision lacks verifiable Delivery Source provenance. Create a new Candidate Revision against a freshly pinned Delivery Source and rerun the complete quality chain."
		} else if errors.Is(err, errDeliverySourceDigestMismatch) {
			reason = "The accepted Candidate Revision's Delivery Source is no longer available at its pinned revision. Create a new Candidate Revision against a freshly pinned Delivery Source and rerun the complete quality chain."
		}
		return c.retryWorkerTransition(ctx, func(isolated []store.WorkerIsolationProof) error {
			return c.Store.RevalidateDeliverySource(ctx, claim, reason, c.now(), isolated...)
		})
	}
	var infrastructureFailure *deliverySourceInfrastructureFailure
	if errors.As(err, &infrastructureFailure) {
		return c.failDeliveryControllerWithClass(ctx, claim, err, store.FailureInfrastructure)
	}
	return c.failDeliveryController(ctx, claim, err)
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
	structured, structuredErr := candidateoutput.ExtractCodexCandidate(output)
	if sessionID == "" {
		sessionID = existing
	}
	if sessionID == "" || structuredErr != nil {
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
