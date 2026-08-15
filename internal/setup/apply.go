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
type Engine struct {
	Adapter          EffectAdapter
	SecretInput      *SecretInput
	Now              func() time.Time
	ResolveCodexAuth func(context.Context) (string, error)
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
	for _, precondition := range plan.Preconditions {
		var checkErr error
		if precondition.Kind == "platform_release" {
			checkErr = checkPlatformReleasePrecondition(ctx, database, plan, precondition)
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
					if e.SecretInput == nil || e.SecretInput.Reader == nil {
						result.Status = setupcontract.ExecutionIncomplete
						result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: verifyErr.Error()})
						err = verifyErr
						break
					}
					if replaceErr := e.Adapter.Apply(ctx, effect, e.SecretInput); replaceErr != nil {
						result.Status = setupcontract.ExecutionIncomplete
						result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: replaceErr.Error()})
						err = replaceErr
						break
					}
					if verifyErr = verifyAndRecordPAT(ctx, database, layout, effect); verifyErr != nil {
						result.Status = setupcontract.ExecutionIncomplete
						result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: verifyErr.Error()})
						err = verifyErr
						break
					}
				}
			}
			if effect.Kind == "repository_admission" {
				if admissionErr := e.recordRepositoryAdmission(ctx, database, layout, plan.Target.RepositoryPath, effect, digest, now, false); admissionErr != nil {
					result.Status = setupcontract.ExecutionIncomplete
					result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: admissionErr.Error()})
					err = admissionErr
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
		if effect.Kind == "repository_admission" {
			if admissionErr := e.recordRepositoryAdmission(ctx, database, layout, plan.Target.RepositoryPath, effect, digest, now, false); admissionErr != nil {
				result.Status = setupcontract.ExecutionIncomplete
				result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: setupcontract.EffectFailed, Evidence: admissionErr.Error()})
				err = admissionErr
				break
			}
		}
		result.Effects = append(result.Effects, setupcontract.EffectResult{EffectID: effect.ID, Status: status, Evidence: evidence})
	}
	if err == nil && plan.Kind == setupcontract.RepositoryOnboarding {
		for _, effect := range plan.Effects {
			if effect.Kind == "repository_admission" {
				if admissionErr := e.recordRepositoryAdmission(ctx, database, layout, plan.Target.RepositoryPath, effect, digest, now, true); admissionErr != nil {
					result.Status = setupcontract.ExecutionIncomplete
					err = admissionErr
				}
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
		for _, effect := range plan.Effects {
			if effect.Kind == "platform_installation" && effect.Parameters["release_manifest_digest"] == precondition.Expected {
				return nil
			}
		}
		return errors.New("Platform Release precondition is not bound to the approved Platform Installation effect")
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
	contractPath := filepath.Join(value.WorkflowHome, "config", "platform-setup-contract.json")
	raw, fileErr := os.ReadFile(contractPath)
	if fileErr != nil || string(raw) != effect.Parameters["platform_setup_contract_json"] {
		return setupcontract.EffectRequired, "installed Platform Setup Contract is absent or differs", nil
	}
	return setupcontract.EffectSatisfied, "Platform Installation matches the approved release", nil
}

func recordPlatformInstallation(ctx context.Context, database *store.Store, layout workflowhome.Layout, effect setupcontract.Effect, now time.Time) error {
	contractRaw := []byte(effect.Parameters["platform_setup_contract_json"])
	var contract platformrelease.PlatformSetupContract
	if len(contractRaw) == 0 || json.Unmarshal(contractRaw, &contract) != nil || contract.Validate() != nil {
		return errors.New("platform installation effect lacks a valid release-declared Platform Setup Contract")
	}
	contractPath := filepath.Join(layout.Config, "platform-setup-contract.json")
	if err := writeAtomic(contractPath, contractRaw); err != nil {
		return err
	}
	return database.RecordPlatformInstallation(ctx, store.PlatformInstallation{PlatformVersion: effect.Parameters["version"], ReleaseManifestDigestSHA256: effect.Parameters["release_manifest_digest"], WorkflowHome: layout.Root, InstalledAt: now, VerifiedAt: now})
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
