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
	"path/filepath"
	"strconv"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/setupcontract"
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
type Engine struct {
	Adapter     EffectAdapter
	SecretInput *SecretInput
	Now         func() time.Time
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
	if e.Adapter == nil {
		return result, errors.New("Setup effect adapter is required")
	}
	for _, effect := range plan.Effects {
		if effect.Kind == "platform_installation" {
			status, evidence, platformErr := readPlatformInstallation(ctx, database, effect)
			if platformErr == nil && status == setupcontract.EffectRequired {
				platformErr = recordPlatformInstallation(ctx, database, layout, effect, now)
				if platformErr == nil {
					status, evidence, platformErr = readPlatformInstallation(ctx, database, effect)
				}
			}
			if platformErr != nil || status != setupcontract.EffectSatisfied {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: evidence})
				err = platformErr
				if err == nil {
					err = errors.New("Platform Installation did not read back")
				}
				break
			}
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
			continue
		}
		status, evidence, readErr := e.Adapter.Readback(ctx, effect)
		if readErr != nil {
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: readErr.Error()})
			err = readErr
			break
		}
		if status == setupcontract.EffectConflicting {
			result.Status = setupcontract.ExecutionDrifted
			result.Blocker = "effect precondition drifted"
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
			err = errors.New(result.Blocker)
			break
		}
		if status == setupcontract.EffectSatisfied {
			if effect.Kind == "github_pat" {
				if verifyErr := verifyAndRecordPAT(ctx, database, layout, effect); verifyErr != nil {
					result.Status = setupcontract.ExecutionIncomplete
					result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: verifyErr.Error()})
					err = verifyErr
					break
				}
			}
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
			continue
		}
		if applyErr := e.Adapter.Apply(ctx, effect, e.SecretInput); applyErr != nil {
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: applyErr.Error()})
			err = applyErr
			break
		}
		status, evidence, readErr = e.Adapter.Readback(ctx, effect)
		if readErr != nil || status != setupcontract.EffectSatisfied {
			if readErr == nil {
				readErr = fmt.Errorf("effect %q did not read back as satisfied", effect.ID)
			}
			result.Status = setupcontract.ExecutionIncomplete
			result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: evidence})
			err = readErr
			break
		}
		if effect.Kind == "github_pat" {
			if verifyErr := verifyAndRecordPAT(ctx, database, layout, effect); verifyErr != nil {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: verifyErr.Error()})
				err = verifyErr
				break
			}
		}
		result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
	}
	result.FinishedAt = time.Now().UTC()
	if e.Now != nil {
		result.FinishedAt = e.Now().UTC()
	}
	encoded, _ := json.Marshal(result.Effects)
	recordErr := database.AppendSetupExecutionResult(ctx, store.SetupExecutionResult{PlanID: plan.PlanID, Attempt: attempt, Status: string(result.Status), EffectsJSON: string(encoded), Diagnostic: result.Blocker, StartedAt: result.StartedAt, CompletedAt: result.FinishedAt})
	return result, errors.Join(err, recordErr)
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

func readPlatformInstallation(ctx context.Context, database *store.Store, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	value, err := database.PlatformInstallation(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return setupcontract.EffectRequired, "Platform Installation is not recorded", nil
	}
	if err != nil {
		return setupcontract.EffectFailed, "", err
	}
	if value.PlatformVersion != effect.Parameters["version"] || value.ReleaseManifestDigestSHA256 != effect.Parameters["release_manifest_digest"] {
		return setupcontract.EffectConflicting, "Platform Installation differs from the approved release", nil
	}
	return setupcontract.EffectSatisfied, "Platform Installation matches the approved release", nil
}

func recordPlatformInstallation(ctx context.Context, database *store.Store, layout workflowhome.Layout, effect setupcontract.Effect, now time.Time) error {
	return database.RecordPlatformInstallation(ctx, store.PlatformInstallation{PlatformVersion: effect.Parameters["version"], ReleaseManifestDigestSHA256: effect.Parameters["release_manifest_digest"], WorkflowHome: layout.Root, InstalledAt: now, VerifiedAt: now})
}

func targetName(plan setupcontract.Plan) string {
	if plan.Target.RepositoryPath != "" {
		return plan.Target.RepositoryPath
	}
	return plan.Target.WorkflowHome
}
