package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/setupeffect"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type effectReadback func(HostAdapter, context.Context, setupcontract.Effect) (setupcontract.EffectStatus, string, error)
type effectApply func(HostAdapter, context.Context, setupcontract.Effect, *SecretInput) error

type effectExecution struct {
	engine   *Engine
	ctx      context.Context
	database *store.Store
	layout   workflowhome.Layout
	plan     setupcontract.Plan
	digest   string
	now      time.Time
}

type engineReadback func(*effectExecution, setupcontract.Effect) (setupcontract.EffectStatus, string, error)
type engineApply func(*effectExecution, setupcontract.Effect) error
type effectAfterSatisfied func(*effectExecution, setupcontract.Effect, bool) error
type effectFinalize func(*effectExecution, setupcontract.Effect) error

type effectHandler struct {
	contract       setupeffect.Descriptor
	readback       effectReadback
	apply          effectApply
	engineReadback engineReadback
	engineApply    engineApply
	afterSatisfied effectAfterSatisfied
	finalize       effectFinalize
	conflict       func() string
}

type engineBehavior struct {
	readback       engineReadback
	apply          engineApply
	afterSatisfied effectAfterSatisfied
	finalize       effectFinalize
	conflict       func() string
}

var effectHandlers = buildEffectHandlers()

func buildEffectHandlers() map[string]effectHandler {
	readbacks := hostReadbackHandlers()
	applies := hostApplyHandlers()
	behaviors := engineBehaviors()
	descriptors := setupeffect.All()
	handlers := make(map[string]effectHandler, len(descriptors))
	for _, descriptor := range descriptors {
		readback, readbackOK := readbacks[descriptor.Kind]
		apply, applyOK := applies[descriptor.Kind]
		behavior, behaviorOK := behaviors[descriptor.Engine]
		if !readbackOK || !applyOK || !behaviorOK || behavior.readback == nil || behavior.apply == nil || behavior.afterSatisfied == nil || behavior.finalize == nil || behavior.conflict == nil {
			panic("incomplete Setup effect handler: " + descriptor.Kind)
		}
		if _, duplicate := handlers[descriptor.Kind]; duplicate {
			panic("duplicate Setup effect handler: " + descriptor.Kind)
		}
		handlers[descriptor.Kind] = effectHandler{
			contract: descriptor, readback: readback, apply: apply,
			engineReadback: behavior.readback, engineApply: behavior.apply,
			afterSatisfied: behavior.afterSatisfied, finalize: behavior.finalize,
			conflict: behavior.conflict,
		}
		delete(readbacks, descriptor.Kind)
		delete(applies, descriptor.Kind)
	}
	if len(readbacks) != 0 || len(applies) != 0 {
		panic("Setup host handler exists without an effect contract")
	}
	return handlers
}

func engineBehaviors() map[setupeffect.EngineSemantics]engineBehavior {
	standardReadback := func(run *effectExecution, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
		return run.engine.Adapter.Readback(run.ctx, effect)
	}
	standardApply := func(run *effectExecution, effect setupcontract.Effect) error {
		return run.engine.Adapter.Apply(run.ctx, effect, run.engine.SecretInput)
	}
	noPost := func(*effectExecution, setupcontract.Effect, bool) error { return nil }
	noFinalize := func(*effectExecution, setupcontract.Effect) error { return nil }
	standardConflict := func() string { return "effect precondition drifted" }
	standard := engineBehavior{readback: standardReadback, apply: standardApply, afterSatisfied: noPost, finalize: noFinalize, conflict: standardConflict}
	return map[setupeffect.EngineSemantics]engineBehavior{
		setupeffect.StandardEffect: standard,
		setupeffect.PlatformInstallEffect: {
			readback: func(run *effectExecution, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
				return readPlatformInstallation(run.ctx, run.database, effect, hasPlatformInstallationTransition(run.plan))
			},
			apply: func(run *effectExecution, effect setupcontract.Effect) error {
				return recordPlatformInstallation(run.ctx, run.database, run.layout, effect, run.now)
			},
			afterSatisfied: noPost,
			finalize:       noFinalize,
			conflict: func() string {
				return "Platform Installation changed outside the approved transition"
			},
		},
		setupeffect.GitHubPATEffect: {
			readback: standardReadback,
			apply:    standardApply,
			afterSatisfied: func(run *effectExecution, effect setupcontract.Effect, allowRepair bool) error {
				verifyErr := verifyAndRecordPAT(run.ctx, run.database, run.layout, effect, run.engine.GitHubCredentialVerifier)
				if verifyErr == nil || !allowRepair {
					return verifyErr
				}
				if run.engine.SecretInput == nil || run.engine.SecretInput.Reader == nil {
					return verifyErr
				}
				if replaceErr := standardApply(run, effect); replaceErr != nil {
					return replaceErr
				}
				return verifyAndRecordPAT(run.ctx, run.database, run.layout, effect, run.engine.GitHubCredentialVerifier)
			},
			finalize: noFinalize,
			conflict: standardConflict,
		},
		setupeffect.ControlPlaneEffect: {
			readback: standardReadback,
			apply: func(run *effectExecution, effect setupcontract.Effect) error {
				if err := authorizeControlPlane(run.ctx, run.database, effect, run.digest); err != nil {
					return err
				}
				return standardApply(run, effect)
			},
			afterSatisfied: func(run *effectExecution, effect setupcontract.Effect, _ bool) error {
				return authorizeControlPlane(run.ctx, run.database, effect, run.digest)
			},
			finalize: noFinalize,
			conflict: standardConflict,
		},
		setupeffect.AdmissionEffect: {
			readback: standardReadback,
			apply:    standardApply,
			afterSatisfied: func(run *effectExecution, effect setupcontract.Effect, _ bool) error {
				recorded, err := repositoryAdmissionRecorded(run.ctx, run.database, run.plan.Target.RepositoryPath, effect, run.digest, false)
				if err != nil || recorded {
					return err
				}
				return run.engine.recordRepositoryAdmission(run.ctx, run.database, run.layout, run.plan.Target.RepositoryPath, effect, run.digest, run.now, false)
			},
			finalize: func(run *effectExecution, effect setupcontract.Effect) error {
				recorded, err := repositoryAdmissionRecorded(run.ctx, run.database, run.plan.Target.RepositoryPath, effect, run.digest, true)
				if err != nil || recorded {
					return err
				}
				return run.engine.recordRepositoryAdmission(run.ctx, run.database, run.layout, run.plan.Target.RepositoryPath, effect, run.digest, run.now, true)
			},
			conflict: standardConflict,
		},
	}
}

type expectedResultHandler func(*effectExecution, setupcontract.ExpectedResult) error

var expectedResultHandlers = map[string]expectedResultHandler{
	"platform_readiness": func(run *effectExecution, expected setupcontract.ExpectedResult) error {
		if run.engine.ExpectedResultVerifier == nil {
			return errors.New("Platform Ready verifier is required")
		}
		return run.engine.ExpectedResultVerifier(run.ctx, run.plan, expected)
	},
	"repository_admission": func(*effectExecution, setupcontract.ExpectedResult) error { return nil },
}

func verifyExpectedResult(run *effectExecution, expected setupcontract.ExpectedResult) error {
	handler, ok := expectedResultHandlers[expected.Kind]
	if !ok {
		return fmt.Errorf("unsupported Setup expected result kind %q", expected.Kind)
	}
	return handler(run, expected)
}
