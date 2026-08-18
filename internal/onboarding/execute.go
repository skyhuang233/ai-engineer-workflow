package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/store"
)

// EffectAdapter is deliberately repository-scoped. Platform installation,
// Docker, credentials, and Control Plane ownership are not representable in
// an Onboarding Plan and therefore cannot be reached through this interface.
type EffectAdapter interface {
	Readback(context.Context, setupcontract.Effect) (setupcontract.EffectStatus, string, error)
	Apply(context.Context, setupcontract.Effect) error
}

type Executor struct {
	Store   *store.Store
	Adapter EffectAdapter
	Now     func() time.Time
}

func (e Executor) Apply(ctx context.Context, raw []byte, approvedDigest, activeBundleDigest string) (setupcontract.ExecutionResult, error) {
	if e.Store == nil || e.Adapter == nil {
		return setupcontract.ExecutionResult{}, errors.New("Repository Onboarding executor requires store and repository adapter")
	}
	plan, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	if plan.Kind != setupcontract.RepositoryOnboarding {
		return setupcontract.ExecutionResult{}, errors.New("only Repository Onboarding Plans are executable")
	}
	if approvedDigest == "" || digest != approvedDigest {
		return setupcontract.ExecutionResult{}, errors.New("Onboarding Plan Digest differs from the exact approved digest")
	}
	if !hasActiveBundleDigest(plan, activeBundleDigest) {
		return setupcontract.ExecutionResult{}, errors.New("Onboarding Plan is not bound to the active Bundle digest")
	}
	lock, err := startup.AcquireRepositoryLock(plan.Target.WorkflowHome, plan.Target.RepositoryPath)
	if err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	defer lock.Close()
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	if err := e.Store.RecordSetupPlan(ctx, store.SetupPlanRecord{PlanID: plan.PlanID, Kind: string(plan.Kind), SchemaVersion: plan.SchemaVersion, Target: plan.Target.RepositoryPath, DigestSHA256: digest, CanonicalJSON: string(canonical), Projection: plan.Target.GitHubRepository, CreatedAt: now}); err != nil {
		return setupcontract.ExecutionResult{}, err
	}
	result := setupcontract.ExecutionResult{SchemaVersion: setupcontract.SchemaVersion, PlanID: plan.PlanID, PlanDigest: digest, AttemptID: fmt.Sprintf("onboarding-%d", now.UnixNano()), StartedAt: now, Status: setupcontract.ExecutionSucceeded}
	for _, effect := range plan.Effects {
		status, evidence, readErr := e.Adapter.Readback(ctx, effect)
		if readErr != nil {
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: readErr.Error()})
			break
		}
		if status == setupcontract.EffectRequired {
			if err := e.Adapter.Apply(ctx, effect); err != nil {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: err.Error()})
				break
			}
			status, evidence, readErr = e.Adapter.Readback(ctx, effect)
		}
		if readErr != nil || status != setupcontract.EffectSatisfied {
			result.Status = setupcontract.ExecutionIncomplete
			if readErr != nil {
				evidence = readErr.Error()
				status = setupcontract.EffectFailed
			}
		}
		result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
		if result.Status != setupcontract.ExecutionSucceeded {
			break
		}
	}
	if result.Status == setupcontract.ExecutionSucceeded {
		if err := setupcontract.VerifyExpectedResults(plan, result.Effects); err != nil {
			result.Status = setupcontract.ExecutionIncomplete
			result.Blocker = err.Error()
		}
	}
	result.FinishedAt = now
	effects, _ := json.Marshal(result.Effects)
	attempt, attemptErr := e.Store.NextSetupExecutionAttempt(ctx, result.PlanID)
	if attemptErr != nil {
		return result, attemptErr
	}
	if err := e.Store.AppendSetupExecutionResult(ctx, store.SetupExecutionResult{PlanID: result.PlanID, Attempt: attempt, Status: string(result.Status), EffectsJSON: string(effects), Diagnostic: result.Blocker, StartedAt: result.StartedAt, CompletedAt: result.FinishedAt}); err != nil {
		return result, err
	}
	return result, nil
}

func hasActiveBundleDigest(plan setupcontract.Plan, digest string) bool {
	for _, precondition := range plan.Preconditions {
		if precondition.Kind == "platform_release" && precondition.Expected == digest {
			return true
		}
	}
	return false
}
