// Package setup executes immutable Setup Plans through explicit readback and
// append-only result recording.
package setup

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/setupeffect"
	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

var ErrDigestMismatch = errors.New("approved Setup Plan digest does not match canonical plan")

type SecretInput struct {
	Reader   io.Reader
	consumed bool
}

func (s *SecretInput) Read() ([]byte, error) {
	if s == nil || s.Reader == nil {
		return nil, errors.New("approved effect requires secret input on standard input")
	}
	if s.consumed {
		return nil, errors.New("secret input was already consumed")
	}
	s.consumed = true
	return io.ReadAll(io.LimitReader(s.Reader, 1024*1024))
}

type EffectAdapter interface {
	Readback(context.Context, setupcontract.Effect) (setupcontract.EffectStatus, string, error)
	Apply(context.Context, setupcontract.Effect, *SecretInput) error
}

type PreconditionChecker interface {
	CheckPrecondition(context.Context, setupcontract.Precondition) error
}
type PreLayoutPreconditionChecker interface {
	CheckPreLayoutPrecondition(context.Context, setupcontract.Precondition) error
}
type EffectResultRestorer interface {
	RestoreEffectResults([]setupcontract.EffectResult) error
}
type RepositoryCreateAttemptRestorer interface {
	RestoreRepositoryCreateAttemptEvents(setupcontract.Effect, []store.SetupRepositoryCreateAttemptEvent) error
}
type RepositoryCreateOutcomeClassifier interface {
	RepositoryCreateOutcomeUnknown(error) bool
}
type ExpectedResultVerifier func(context.Context, setupcontract.Plan, setupcontract.ExpectedResult) error
type PlatformPreconditionVerifier func(context.Context, setupcontract.Plan) error
type Engine struct {
	Adapter                      EffectAdapter
	SecretInput                  *SecretInput
	Now                          func() time.Time
	ResolveCodexAuth             func(context.Context) (string, error)
	ExpectedResultVerifier       ExpectedResultVerifier
	PlatformPreconditionVerifier PlatformPreconditionVerifier
}

func (e *Engine) Apply(ctx context.Context, raw []byte, approvedDigest string) (setupcontract.ExecutionResult, error) {
	plan, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	if len(digest) != len(approvedDigest) || subtle.ConstantTimeCompare([]byte(digest), []byte(approvedDigest)) != 1 {
		return setupcontract.ExecutionResult{}, ErrDigestMismatch
	}
	layout, err := workflowhome.Resolve(plan.Target.WorkflowHome)
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	if e.Adapter == nil {
		return setupcontract.ExecutionResult{}, errors.New("Setup effect adapter is required")
	}
	// Fence a generated Platform Plan to its approved Windows user before even
	// creating Workflow Home. The ordinary precondition pass repeats the check
	// after layout creation and verifies the resulting/existing directory owner.
	for _, precondition := range plan.Preconditions {
		if precondition.Kind != "host_identity" {
			continue
		}
		checker, ok := e.Adapter.(PreLayoutPreconditionChecker)
		if !ok {
			return setupcontract.ExecutionResult{}, errors.New("Setup adapter cannot verify the host identity before creating Workflow Home")
		}
		if err := checker.CheckPreLayoutPrecondition(ctx, precondition); err != nil {
			return setupcontract.ExecutionResult{}, err
		}
	}
	if err := layout.Ensure(); err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	lock, err := startup.AcquireWorkflowHomeLock(layout.Root)
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	defer lock.Close()
	database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	defer database.Close()
	projection := Project(plan, digest)
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	if err := database.RecordSetupPlan(ctx, store.SetupPlanRecord{PlanID: plan.PlanID, Kind: string(plan.Kind), SchemaVersion: plan.SchemaVersion, Target: targetName(plan), DigestSHA256: digest, CanonicalJSON: string(canonical), Projection: projection, CreatedAt: now}); err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	prior, err := database.SetupExecutionResults(ctx, plan.PlanID)
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	attempt := len(prior) + 1
	result := setupcontract.ExecutionResult{SchemaVersion: 1, PlanID: plan.PlanID, PlanDigest: digest, AttemptID: "attempt-" + strconv.Itoa(attempt), StartedAt: now, Status: setupcontract.ExecutionSucceeded}
	if restorer, ok := e.Adapter.(RepositoryCreateAttemptRestorer); ok {
		for _, effect := range plan.Effects {
			if effect.Kind != "create_repository" {
				continue
			}
			events, eventsErr := database.SetupRepositoryCreateAttemptEvents(ctx, plan.PlanID, effect.ID)
			if eventsErr != nil {
				return result, fmt.Errorf("restore repository-create attempt evidence: %w", eventsErr)
			}
			if err := restorer.RestoreRepositoryCreateAttemptEvents(effect, events); err != nil {
				return result, err
			}
		}
	}
	if restorer, ok := e.Adapter.(EffectResultRestorer); ok {
		for _, previous := range prior {
			var effects []setupcontract.EffectResult
			if err := json.Unmarshal([]byte(previous.EffectsJSON), &effects); err != nil {
				return result, fmt.Errorf("restore prior Setup effect evidence: %w", err)
			}
			if err := restorer.RestoreEffectResults(effects); err != nil {
				return result, err
			}
		}
	}
	for _, precondition := range plan.Preconditions {
		var checkErr error
		if precondition.Kind == "platform_release" {
			checkErr = checkPlatformReleasePrecondition(ctx, database, plan, precondition)
		} else if precondition.Kind == "platform_setup_contract" {
			checkErr = checkPlatformSetupContractPrecondition(plan, precondition)
		} else if precondition.Kind == "platform_installation" {
			checkErr = checkPlatformInstallationPrecondition(ctx, database, plan, precondition)
		} else if checker, ok := e.Adapter.(PreconditionChecker); ok {
			checkErr = checker.CheckPrecondition(ctx, precondition)
		} else {
			checkErr = fmt.Errorf("Setup adapter cannot verify precondition kind %q", precondition.Kind)
		}
		if checkErr != nil {
			result.Status = setupcontract.ExecutionDrifted
			result.Blocker = checkErr.Error()
			result.FinishedAt = time.Now().UTC()
			encoded, _ := json.Marshal(result.Effects)
			recordErr := database.AppendSetupExecutionResult(ctx, store.SetupExecutionResult{PlanID: plan.PlanID, Attempt: attempt, Status: string(result.Status), EffectsJSON: string(encoded), Diagnostic: result.Blocker, StartedAt: result.StartedAt, CompletedAt: result.FinishedAt})
			return result, errors.Join(checkErr, recordErr)
		}
	}
	if plan.Kind == setupcontract.PlatformBootstrap {
		if e.PlatformPreconditionVerifier == nil {
			err = errors.New("Platform component precondition verifier is required")
		} else {
			err = e.PlatformPreconditionVerifier(ctx, plan)
		}
		if err != nil {
			result.Status = setupcontract.ExecutionDrifted
			result.Blocker = err.Error()
			result.FinishedAt = time.Now().UTC()
			encoded, _ := json.Marshal(result.Effects)
			recordErr := database.AppendSetupExecutionResult(ctx, store.SetupExecutionResult{PlanID: plan.PlanID, Attempt: attempt, Status: string(result.Status), EffectsJSON: string(encoded), Diagnostic: result.Blocker, StartedAt: result.StartedAt, CompletedAt: result.FinishedAt})
			return result, errors.Join(err, recordErr)
		}
	}
	for _, effect := range plan.Effects {
		handler, ok := effectHandlers[effect.Kind]
		if !ok {
			result.Status = setupcontract.ExecutionIncomplete
			err = fmt.Errorf("unsupported Setup effect kind %q", effect.Kind)
			break
		}
		run := &effectExecution{engine: e, ctx: ctx, database: database, layout: layout, plan: plan, digest: digest, now: now}
		status, evidence, readErr := handler.engineReadback(run, effect)
		if readErr != nil {
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: readErr.Error()})
			err = readErr
			break
		}
		if status == setupcontract.EffectConflicting {
			result.Status = setupcontract.ExecutionDrifted
			result.Blocker = handler.conflict()
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
			err = errors.New(result.Blocker)
			break
		}
		if status == setupcontract.EffectSatisfied {
			if postErr := handler.afterSatisfied(run, effect, true); postErr != nil {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: postErr.Error()})
				err = postErr
				break
			}
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
			continue
		}
		if effect.Kind == "create_repository" {
			if recordErr := appendRepositoryCreateAttemptEvent(run, effect, attempt, store.RepositoryCreateStarted); recordErr != nil {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: recordErr.Error()})
				err = recordErr
				break
			}
		}
		if applyErr := handler.engineApply(run, effect); applyErr != nil {
			if effect.Kind == "create_repository" {
				outcome := store.RepositoryCreateDefinitiveFailure
				if classifier, ok := e.Adapter.(RepositoryCreateOutcomeClassifier); ok && classifier.RepositoryCreateOutcomeUnknown(applyErr) {
					outcome = store.RepositoryCreateOutcomeUnknown
				}
				applyErr = errors.Join(applyErr, appendRepositoryCreateAttemptEvent(run, effect, attempt, outcome))
			}
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: applyErr.Error()})
			err = applyErr
			break
		}
		if effect.Kind == "create_repository" {
			if recordErr := appendRepositoryCreateAttemptEvent(run, effect, attempt, store.RepositoryCreateSucceeded); recordErr != nil {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: recordErr.Error()})
				err = recordErr
				break
			}
		}
		status, evidence, readErr = handler.engineReadback(run, effect)
		if readErr != nil || status != setupcontract.EffectSatisfied {
			if readErr == nil {
				readErr = fmt.Errorf("effect %q did not read back as satisfied", effect.ID)
			}
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: evidence})
			err = readErr
			break
		}
		if postErr := handler.afterSatisfied(run, effect, false); postErr != nil {
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: postErr.Error()})
			err = postErr
			break
		}
		result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
	}
	if err == nil {
		run := &effectExecution{engine: e, ctx: ctx, database: database, layout: layout, plan: plan, digest: digest, now: now}
		for _, effect := range plan.Effects {
			handler, ok := effectHandlers[effect.Kind]
			if !ok {
				err = fmt.Errorf("unsupported Setup effect kind %q", effect.Kind)
				break
			}
			if finalizeErr := handler.finalize(run, effect); finalizeErr != nil {
				result.Status = setupcontract.ExecutionIncomplete
				err = finalizeErr
				break
			}
		}
	}
	if err == nil {
		if expectedErr := setupcontract.VerifyExpectedResults(plan, result.Effects); expectedErr != nil {
			result.Status = setupcontract.ExecutionIncomplete
			err = expectedErr
		}
	}
	if err == nil {
		run := &effectExecution{engine: e, ctx: ctx, database: database, layout: layout, plan: plan, digest: digest, now: now}
		for _, expected := range plan.ExpectedResults {
			err = verifyExpectedResult(run, expected)
			if err != nil {
				result.Status = setupcontract.ExecutionIncomplete
				break
			}
		}
	}
	result.FinishedAt = time.Now().UTC()
	if e.Now != nil {
		result.FinishedAt = e.Now().UTC()
	}
	encoded, _ := json.Marshal(result.Effects)
	recordErr := database.AppendSetupExecutionResult(ctx, store.SetupExecutionResult{PlanID: plan.PlanID, Attempt: attempt, Status: string(result.Status), EffectsJSON: string(encoded), Diagnostic: result.Blocker, StartedAt: result.StartedAt, CompletedAt: result.FinishedAt})
	return result, errors.Join(err, recordErr)
}

func appendRepositoryCreateAttemptEvent(run *effectExecution, effect setupcontract.Effect, executionAttempt int, event store.RepositoryCreateAttemptEvent) error {
	return run.database.AppendSetupRepositoryCreateAttemptEvent(run.ctx, store.SetupRepositoryCreateAttemptEvent{
		PlanID: run.plan.PlanID, PlanDigestSHA256: run.digest, EffectID: effect.ID, ExecutionAttempt: executionAttempt, Event: event,
		Owner: effect.Parameters["owner"], Name: effect.Parameters["name"], Private: effect.Parameters["private"] == "true",
		ApprovalAbsentRepository: effect.Parameters["approval_absent_repository"], RecordedAt: run.now,
	})
}

func repositoryAdmissionRecorded(ctx context.Context, database *store.Store, repositoryPath string, effect setupcontract.Effect, planDigest string, requireEligible bool) (bool, error) {
	admissionValue, err := database.RepositoryAdmission(ctx, effect.Subject)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if admissionValue.OnboardingPlanDigestSHA256 != planDigest || admissionValue.ContractVersion != effect.Parameters["contract_version"] || admissionValue.ManifestDigestSHA256 != effect.Parameters["manifest_digest"] || requireEligible && !admissionValue.Eligible {
		return false, nil
	}
	runtime, err := database.RepositoryRuntimeConfiguration(ctx, effect.Subject)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if runtime.DefaultBranch != effect.Parameters["default_branch"] || runtime.SourcePath != repositoryPath || runtime.WorkspaceRoot == "" || runtime.StateRoot == "" || runtime.CodexAuthFile == "" {
		return false, nil
	}
	return true, nil
}

func (e *Engine) recordRepositoryAdmission(ctx context.Context, database *store.Store, layout workflowhome.Layout, repositoryPath string, effect setupcontract.Effect, planDigest string, now time.Time, eligible bool) error {
	value := store.RepositoryAdmission{
		Repository: effect.Subject, OnboardingPlanDigestSHA256: planDigest,
		ContractVersion: effect.Parameters["contract_version"], ManifestDigestSHA256: effect.Parameters["manifest_digest"],
		Eligible: false, SuspensionReason: "Repository Onboarding is incomplete", VerifiedAt: now,
	}
	// The runtime configuration references the admission. Persist only a
	// scheduler-ineligible admission until every dependent record exists.
	if err := database.RecordRepositoryAdmission(ctx, value); err != nil {
		return err
	}
	resolveAuth := e.ResolveCodexAuth
	if resolveAuth == nil {
		resolveAuth = codexauth.ResolveChatGPT
	}
	authFile, err := resolveAuth(ctx)
	if err != nil {
		return fmt.Errorf("resolve Codex Authentication Source: %w", err)
	}
	repositoryKey := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.ToLower(effect.Subject))
	if err := database.RecordRepositoryRuntimeConfiguration(ctx, store.RepositoryRuntimeConfiguration{
		Repository: effect.Subject, DefaultBranch: effect.Parameters["default_branch"], SourcePath: repositoryPath,
		WorkspaceRoot: filepath.Join(layout.Workspaces, repositoryKey), StateRoot: filepath.Join(layout.State, "codex", repositoryKey), CodexAuthFile: authFile,
		GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: 7 * 24 * time.Hour, MaxParallelRuns: 1, UpdatedAt: now,
	}); err != nil {
		return err
	}
	value.Eligible = eligible
	if eligible {
		value.SuspensionReason = ""
	}
	return database.RecordRepositoryAdmission(ctx, value)
}

func checkPlatformReleasePrecondition(ctx context.Context, database *store.Store, plan setupcontract.Plan, precondition setupcontract.Precondition) error {
	if plan.Kind == setupcontract.PlatformBootstrap {
		found := false
		for _, effect := range plan.Effects {
			if !isPlatformMutationEffect(effect.Kind) {
				continue
			}
			found = true
			if effect.Parameters["release_manifest_digest"] != precondition.Expected {
				return errors.New("Platform Release precondition is not bound to every approved platform effect")
			}
		}
		if found {
			return nil
		}
		return errors.New("Platform Release precondition is not bound to an approved platform effect")
	}
	installation, err := database.PlatformInstallation(ctx)
	if err != nil {
		return err
	}
	if installation.ReleaseManifestDigestSHA256 != precondition.Expected {
		return errors.New("Platform Release precondition drifted")
	}
	return nil
}

func checkPlatformSetupContractPrecondition(plan setupcontract.Plan, precondition setupcontract.Precondition) error {
	if plan.Kind != setupcontract.PlatformBootstrap {
		return errors.New("Platform Setup Contract precondition is valid only for Platform Bootstrap")
	}
	found := false
	for _, effect := range plan.Effects {
		if !isPlatformMutationEffect(effect.Kind) {
			continue
		}
		found = true
		if effect.Parameters["platform_setup_contract_digest"] != precondition.Expected {
			return errors.New("Platform Setup Contract precondition is not bound to every approved platform effect")
		}
	}
	if found {
		return nil
	}
	return errors.New("Platform Setup Contract precondition is not bound to an approved platform effect")
}

func checkPlatformInstallationPrecondition(ctx context.Context, database *store.Store, plan setupcontract.Plan, precondition setupcontract.Precondition) error {
	if plan.Kind != setupcontract.PlatformBootstrap || precondition.Subject != plan.Target.WorkflowHome {
		return errors.New("Platform Installation transition precondition is not bound to its Workflow Home")
	}
	var approvedEffect *setupcontract.Effect
	for _, effect := range plan.Effects {
		handler, ok := effectHandlers[effect.Kind]
		if ok && handler.contract.Engine == setupeffect.PlatformInstallEffect {
			if approvedEffect != nil || effect.Subject != plan.Target.WorkflowHome {
				return errors.New("Platform Installation transition must bind one exact approved record effect")
			}
			copy := effect
			approvedEffect = &copy
		}
	}
	if approvedEffect == nil {
		return errors.New("Platform Installation transition precondition has no approved record effect")
	}
	installation, err := database.PlatformInstallation(ctx)
	if err != nil {
		return err
	}
	_, actualDigest, err := setupcontract.Canonicalize(platformInstallationStateJSON(installation))
	if err != nil {
		return errors.Join(errors.New("Platform Installation transition source pins drifted"), err)
	}
	if actualDigest == precondition.Expected {
		return nil
	}
	// A previous attempt may have durably recorded the approved installation
	// before a later Control Plane effect failed. Accept only the exact state
	// that this same approved record effect writes (authorization is still
	// blank at that boundary); any third state remains drift.
	approvedNew := store.PlatformInstallation{
		PlatformVersion:                   approvedEffect.Parameters["version"],
		ReleaseManifestDigestSHA256:       approvedEffect.Parameters["release_manifest_digest"],
		PlatformSetupContractDigestSHA256: approvedEffect.Parameters["platform_setup_contract_digest"],
		WorkflowCLISHA256:                 approvedEffect.Parameters["workflow_cli_sha256"],
		ReleaseBundledFilesDigestSHA256:   approvedEffect.Parameters["release_bundled_files_digest"],
	}
	_, approvedNewDigest, newErr := setupcontract.Canonicalize(platformInstallationStateJSON(approvedNew))
	if newErr == nil && actualDigest == approvedNewDigest && platformInstallationEffectPreviouslySatisfied(ctx, database, plan.PlanID, approvedEffect.ID) {
		return nil
	}
	return errors.Join(errors.New("Platform Installation transition source pins drifted"), newErr)
}

func platformInstallationEffectPreviouslySatisfied(ctx context.Context, database *store.Store, planID, effectID string) bool {
	results, err := database.SetupExecutionResults(ctx, planID)
	if err != nil {
		return false
	}
	for _, result := range results {
		var effects []setupcontract.EffectResult
		if json.Unmarshal([]byte(result.EffectsJSON), &effects) != nil {
			continue
		}
		for _, effect := range effects {
			if effect.EffectID == effectID && effect.Status == setupcontract.EffectSatisfied {
				return true
			}
		}
	}
	return false
}

func hasPlatformInstallationTransition(plan setupcontract.Plan) bool {
	for _, precondition := range plan.Preconditions {
		if precondition.Kind == "platform_installation" {
			return true
		}
	}
	return false
}

func platformInstallationStateJSON(value store.PlatformInstallation) []byte {
	raw, _ := json.Marshal(map[string]string{
		"version": value.PlatformVersion, "release_manifest_digest": value.ReleaseManifestDigestSHA256,
		"platform_setup_contract_digest": value.PlatformSetupContractDigestSHA256, "workflow_cli_sha256": value.WorkflowCLISHA256,
		"release_bundled_files_digest": value.ReleaseBundledFilesDigestSHA256, "control_plane_plan_digest_sha256": value.ControlPlanePlanDigestSHA256,
	})
	return raw
}

func isPlatformMutationEffect(kind string) bool {
	handler, ok := effectHandlers[kind]
	return ok && handler.contract.PlatformMutation
}

func verifyAndRecordPAT(ctx context.Context, database *store.Store, layout workflowhome.Layout, effect setupcontract.Effect) error {
	owner := effect.Parameters["owner"]
	if owner == "" {
		return errors.New("GitHub PAT effect requires an owner binding")
	}
	token, err := credential.NewFileStore(layout.CredentialFile).Get(ctx, credential.GatewayTarget)
	if err != nil {
		return err
	}
	verification, err := (githubcredential.Verifier{APIBase: effect.Parameters["api_base"]}).Verify(ctx, token, owner)
	if err != nil {
		return err
	}
	return database.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: verification.FingerprintSHA256, Login: verification.Login, UserID: verification.UserID, Owner: verification.Owner, Scopes: verification.Scopes, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: verification.VerifiedAt})
}

func readPlatformInstallation(ctx context.Context, database *store.Store, effect setupcontract.Effect, allowTransition bool) (setupcontract.EffectStatus, string, error) {
	value, err := database.PlatformInstallation(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return setupcontract.EffectRequired, "Platform Installation is not recorded", nil
	}
	if err != nil {
		return setupcontract.EffectFailed, "", err
	}
	if value.PlatformVersion != effect.Parameters["version"] || value.ReleaseManifestDigestSHA256 != effect.Parameters["release_manifest_digest"] {
		if allowTransition {
			return setupcontract.EffectRequired, "approved Platform Installation release transition is required", nil
		}
		return setupcontract.EffectConflicting, "Platform Installation differs from the approved release", nil
	}
	if value.PlatformSetupContractDigestSHA256 == "" || value.WorkflowCLISHA256 == "" || value.ReleaseBundledFilesDigestSHA256 == "" || value.ReleaseBundledFilesJSON == "" {
		return setupcontract.EffectRequired, "Platform Installation lacks durable verified release pins", nil
	}
	if value.PlatformSetupContractDigestSHA256 != effect.Parameters["platform_setup_contract_digest"] || value.WorkflowCLISHA256 != effect.Parameters["workflow_cli_sha256"] || value.ReleaseBundledFilesDigestSHA256 != effect.Parameters["release_bundled_files_digest"] || value.ReleaseBundledFilesJSON != effect.Parameters["release_bundled_files_json"] {
		if allowTransition {
			return setupcontract.EffectRequired, "approved Platform Installation pin transition is required", nil
		}
		return setupcontract.EffectConflicting, "Platform Installation durable release pins differ", nil
	}
	contractPath := filepath.Join(value.WorkflowHome, "config", "platform-setup-contract.json")
	raw, fileErr := os.ReadFile(contractPath)
	canonical, contractDigest, canonicalErr := setupcontract.Canonicalize(raw)
	if fileErr != nil || canonicalErr != nil || string(canonical) != effect.Parameters["platform_setup_contract_json"] || contractDigest != effect.Parameters["platform_setup_contract_digest"] {
		return setupcontract.EffectRequired, "installed Platform Setup Contract is absent or differs", nil
	}
	verifiedCLI, verifyErr := (workflowhome.Installation{Layout: workflowhome.Layout{Root: value.WorkflowHome, Bin: filepath.Join(value.WorkflowHome, "bin")}}).VerifyVersion(effect.Parameters["version"], effect.Parameters["workflow_cli_sha256"])
	if verifyErr != nil {
		return setupcontract.EffectFailed, "", verifyErr
	}
	if !verifiedCLI {
		return setupcontract.EffectRequired, "installed Workflow CLI ownership, version, or checksum differs", nil
	}
	return setupcontract.EffectSatisfied, "Platform Installation matches the approved release", nil
}

func recordPlatformInstallation(ctx context.Context, database *store.Store, layout workflowhome.Layout, effect setupcontract.Effect, now time.Time) error {
	contractRaw := []byte(effect.Parameters["platform_setup_contract_json"])
	canonicalContract, contractDigest, canonicalErr := setupcontract.Canonicalize(contractRaw)
	var contract platformrelease.PlatformSetupContract
	if len(contractRaw) == 0 || canonicalErr != nil || string(canonicalContract) != string(contractRaw) || contractDigest != effect.Parameters["platform_setup_contract_digest"] || json.Unmarshal(contractRaw, &contract) != nil || contract.Validate() != nil {
		return errors.New("platform installation effect lacks a valid release-declared Platform Setup Contract")
	}
	bundledFilesRaw := []byte(effect.Parameters["release_bundled_files_json"])
	canonicalBundledFiles, bundledFilesDigest, bundledFilesErr := setupcontract.Canonicalize(bundledFilesRaw)
	var bundledFiles []platformrelease.BundledFile
	if len(bundledFilesRaw) == 0 || bundledFilesErr != nil || string(canonicalBundledFiles) != string(bundledFilesRaw) || bundledFilesDigest != effect.Parameters["release_bundled_files_digest"] || json.Unmarshal(bundledFilesRaw, &bundledFiles) != nil || len(bundledFiles) == 0 {
		return errors.New("platform installation effect lacks the signed release bundled-file inventory")
	}
	contractPath := filepath.Join(layout.Config, "platform-setup-contract.json")
	if err := writeAtomic(contractPath, contractRaw); err != nil {
		return err
	}
	return database.RecordPlatformInstallation(ctx, store.PlatformInstallation{PlatformVersion: effect.Parameters["version"], ReleaseManifestDigestSHA256: effect.Parameters["release_manifest_digest"], PlatformSetupContractDigestSHA256: effect.Parameters["platform_setup_contract_digest"], WorkflowCLISHA256: effect.Parameters["workflow_cli_sha256"], ReleaseBundledFilesJSON: string(canonicalBundledFiles), ReleaseBundledFilesDigestSHA256: bundledFilesDigest, WorkflowHome: layout.Root, InstalledAt: now, VerifiedAt: now})
}

func authorizeControlPlane(ctx context.Context, database *store.Store, effect setupcontract.Effect, planDigest string) error {
	return database.AuthorizeControlPlane(ctx, store.PlatformInstallation{PlatformVersion: effect.Parameters["version"], ReleaseManifestDigestSHA256: effect.Parameters["release_manifest_digest"], PlatformSetupContractDigestSHA256: effect.Parameters["platform_setup_contract_digest"], WorkflowCLISHA256: effect.Parameters["workflow_cli_sha256"], ReleaseBundledFilesDigestSHA256: effect.Parameters["release_bundled_files_digest"]}, planDigest)
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func targetName(plan setupcontract.Plan) string {
	if plan.Target.RepositoryPath != "" {
		return plan.Target.RepositoryPath
	}
	return plan.Target.WorkflowHome
}
