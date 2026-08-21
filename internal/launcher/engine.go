package launcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/store"
)

const (
	activeRepairRequired = "repair_required"
	activeReady          = "ready"
)

type Active struct {
	SchemaVersion int       `json:"schema_version"`
	Generation    string    `json:"generation"`
	Version       string    `json:"version"`
	BundleDigest  string    `json:"bundle_digest"`
	AttemptID     string    `json:"attempt_id"`
	ConsentID     string    `json:"consent_id"`
	Readiness     string    `json:"readiness"`
	ActivatedAt   time.Time `json:"activated_at"`
}

type Attempt struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	SourceAttempt string    `json:"source_attempt,omitempty"`
	TargetVersion string    `json:"target_version"`
	BundleDigest  string    `json:"bundle_digest"`
	Generation    string    `json:"generation"`
	ConsentID     string    `json:"consent_id"`
	Phase         string    `json:"phase"`
	CreatedAt     time.Time `json:"created_at"`
	Diagnostics   string    `json:"diagnostics,omitempty"`
}

type Consent struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	TargetVersion string       `json:"target_version"`
	BundleDigest  string       `json:"bundle_digest"`
	WorkflowHome  string       `json:"workflow_home"`
	Capabilities  []Capability `json:"capabilities"`
	GitHubOwner   string       `json:"github_owner,omitempty"`
	AcceptedAt    time.Time    `json:"accepted_at"`
}

type Engine struct {
	BundleRoot string
	Now        func() time.Time
	// ReconcilePath performs the current-user PATH mutation after consent. It
	// is injected so protocol and filesystem tests never touch a real profile.
	ReconcilePath func(string) error
	Lifecycle     Lifecycle
	// DependencyInspector is the read-only seam used during inspect/apply to
	// bind Docker consent to what is actually present on this host.
	DependencyInspector DependencyInspector
	VerifyPAT           func(context.Context, string, string) (githubcredential.Verification, error)
	// AfterConsentRecorded is a test seam called immediately after the Consent
	// record becomes the first durable state in a previously missing Workflow
	// Home. Production callers leave it nil.
	AfterConsentRecorded func(string)
	// AfterConsentWritten is a crash-injection seam between durable Consent and
	// its immediately following Attempt. Production callers leave it nil.
	AfterConsentWritten func(string)
	// BeforeRecoveryLock is a test seam called after a fresh recovery is
	// discovered but before its in-Home lock is acquired and revalidated.
	// Production callers leave it nil.
	BeforeRecoveryLock func(string)
}

// Lifecycle is the only boundary through which Launcher controls external
// dependencies and the single active Control Plane. Implementations perform
// Docker reuse/install/start/verify, immutable image pull, process stop/start,
// and live identity/readiness checks; tests inject a deterministic fake.
type Lifecycle interface {
	Prepare(context.Context, Request, Consent) error
	Stop(context.Context, string, Active) error
	Start(context.Context, string, Active) error
	Ready(context.Context, string, Active) error
}

type DependencyInspector interface {
	DockerVersion(context.Context) (string, error)
}

type restartableLifecycle interface {
	Restart(context.Context, string, Active) error
}

func (e Engine) Inspect(ctx context.Context, request Request) (Result, error) {
	_ = ctx
	if request.Purpose == PurposeTargetState {
		if _, err := e.workerStateFromVerifiedWorkflowRelease(request); err != nil {
			return blocked(err), nil
		}
	}
	if request.Purpose == PurposeActiveWorkPreflight {
		active, err := ReadActive(request.WorkflowHome)
		if err != nil {
			return blocked(err), nil
		}
		count, ids, err := activeRuns(filepath.Join(request.WorkflowHome, "platform", "generations", active.Generation, "workflow.db"))
		if err != nil {
			return blocked(err), nil
		}
		evidence := map[string]any{"active": active, "active_worker_run_count": count, "active_worker_run_ids": ids}
		if count > 0 {
			return result("active_work", evidence), nil
		}
		return result("ready", evidence), nil
	}
	required, err := e.requiredCapabilities(ctx, request, nil)
	if err != nil {
		return result("consent_required", map[string]any{"reason": "github_owner_required"}), nil
	}
	if _, err := ReadActive(request.WorkflowHome); err == nil {
		// A target launcher only returns a reusable Consent after the exact
		// target has been requested; healthy reuse belongs to dispatcher verify.
		if consent, ok := reusableConsent(request.WorkflowHome, request.TargetVersion, request.BundleDigest, required); ok {
			return result("ready", map[string]any{"disposition": "repair", "consent_id": consent.ID}), nil
		}
	}
	if consent, attempt, ok := e.reusableFreshRecovery(request.WorkflowHome, request.TargetVersion, request.BundleDigest, required); ok {
		return result("ready", map[string]any{"disposition": "repair", "consent_id": consent.ID, "attempt": attempt}), nil
	}
	return result("consent_required", map[string]any{"disposition": "apply", "required_capabilities": required}), nil
}

func (e Engine) Apply(ctx context.Context, request Request) (Result, error) {
	// A rejected request must not turn an absent Workflow Home into a partial
	// installation. Validate both the protocol shape and the concrete consent
	// target before creating either the Home or its in-Home installation lock.
	if err := validateApplyRequest(request); err != nil {
		return blocked(err), nil
	}
	verifiedRelease, err := e.workerStateFromVerifiedWorkflowRelease(request)
	if err != nil {
		return blocked(err), nil
	}
	recoveryConsent, recoveryAttempt, recoveringFresh, recoveryErr := e.freshRecoveryForApply(ctx, request)
	if recoveryErr != nil {
		return blocked(recoveryErr), nil
	}
	if !recoveringFresh {
		if err := validateHomeForApply(request.WorkflowHome); err != nil {
			return blocked(err), nil
		}
	}
	if preflight, invalid := e.preflightConsent(ctx, request); invalid {
		return preflight, nil
	}

	freshHome, err := workflowHomeFresh(request.WorkflowHome)
	if err != nil {
		return blocked(err), nil
	}
	if freshHome {
		// The prospective lock deliberately lives outside Workflow Home. It
		// serializes two first installers without making setup.lock the first
		// durable record in the Home.
		unlockProspective, lockErr := acquireProspectiveLock(request.WorkflowHome)
		if lockErr != nil {
			return blocked(lockErr), nil
		}
		defer unlockProspective()
		if err := validateHomeForApply(request.WorkflowHome); err != nil {
			return blocked(err), nil
		}
		if fresh, freshErr := workflowHomeFresh(request.WorkflowHome); freshErr != nil {
			return blocked(freshErr), nil
		} else if !fresh {
			return blocked(errors.New("Workflow Home changed while awaiting first-install lock")), nil
		}
		// Re-read capabilities/consent under the process-shared lock. This
		// protects callers which calculated consent while another installer was
		// finishing and, crucially, still performs no Home mutation on failure.
		if preflight, invalid := e.preflightConsent(ctx, request); invalid {
			return preflight, nil
		}
	} else {
		if recoveringFresh && e.BeforeRecoveryLock != nil {
			e.BeforeRecoveryLock(request.WorkflowHome)
		}
		unlock, lockErr := acquireLock(request.WorkflowHome)
		if lockErr != nil {
			return blocked(lockErr), nil
		}
		defer unlock()
		// A recovery record is untrusted until it is discovered again while the
		// in-Home lock is held.  Do not let the pre-lock snapshot authorize
		// cleanup or staging after another process has changed the layout.
		if recoveringFresh {
			lockedConsent, lockedAttempt, lockedRecovery, lockedErr := e.freshRecoveryForApply(ctx, request)
			if lockedErr != nil || !lockedRecovery {
				if lockedErr == nil {
					lockedErr = errors.New("fresh recovery layout changed while awaiting lock")
				}
				return blocked(lockedErr), nil
			}
			recoveryConsent, recoveryAttempt = lockedConsent, lockedAttempt
		}
		if preflight, invalid := e.preflightConsent(ctx, request); invalid {
			return preflight, nil
		}
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	active, activeErr := ReadActive(request.WorkflowHome)
	resumeActiveRepair := activeErr == nil && active.Readiness == activeRepairRequired && active.Version == request.TargetVersion && active.BundleDigest == request.BundleDigest
	// On an existing ready installation, this atomic fence/count is the first
	// platform coordination after taking the installation lock.  In
	// particular, active work returns before Consent, credentials, dependencies
	// or any payload/PATH mutation can be written.
	operation := ""
	var oldDB *store.Store
	if activeErr == nil && !resumeActiveRepair {
		operation = randomID("maintenance-")
		// The old active generation is only inspected/fenced. It must never be
		// created or migrated by a target Launcher before the active-work count
		// transaction; any schema change belongs solely to the candidate copy.
		oldDB, err = store.OpenForLauncherMaintenance(context.Background(), filepath.Join(request.WorkflowHome, "platform", "generations", active.Generation, "workflow.db"))
		if err != nil {
			return blocked(err), nil
		}
		defer oldDB.Close()
		count, fenceErr := oldDB.BeginMaintenanceFence(ctx, store.MaintenanceFence{OperationID: operation, BundleDigest: request.BundleDigest}, now)
		if fenceErr != nil {
			return Result{}, fenceErr
		}
		if count > 0 {
			return result("active_work", map[string]any{"active_worker_run_count": count}), nil
		}
	}
	oldStopped := false
	activationCommitted := false
	var priorCredential []byte
	credentialWasPresent := false
	credentialReplaced := false
	defer func() {
		if credentialReplaced && !activationCommitted {
			credentialPath := filepath.Join(request.WorkflowHome, "state", "credentials", "github.pat")
			if credentialWasPresent {
				_ = writeAtomic(credentialPath, priorCredential, 0o600)
			} else {
				_ = os.Remove(credentialPath)
			}
		}
		if oldDB != nil && !activationCommitted {
			_ = oldDB.ClearMaintenanceFence(context.Background(), operation)
			if oldStopped && e.Lifecycle != nil {
				if restartable, ok := e.Lifecycle.(restartableLifecycle); ok {
					_ = restartable.Restart(context.Background(), request.WorkflowHome, active)
				}
			}
		}
	}()
	var consent Consent
	var required []Capability
	if request.ConsentID != "" {
		consent, err = readConsent(request.WorkflowHome, request.ConsentID)
		if err != nil {
			return result("consent_required", map[string]any{"reason": "consent_not_found"}), nil
		}
		required, err = e.requiredCapabilities(ctx, request, &consent)
		if err != nil {
			return result("consent_required", map[string]any{"reason": "github_owner_required"}), nil
		}
		if consent.TargetVersion != request.TargetVersion || consent.BundleDigest != request.BundleDigest || !e.sameRecoveryCapabilities(consent.Capabilities, required) {
			return result("consent_required", map[string]any{"reason": "consent_target_changed", "required_capabilities": required}), nil
		}
	} else {
		required, err = e.requiredCapabilities(ctx, request, nil)
		if err != nil {
			return result("consent_required", map[string]any{"reason": "github_owner_required"}), nil
		}
		consent = Consent{SchemaVersion: ProtocolVersion, ID: randomID("consent-"), TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, WorkflowHome: request.WorkflowHome, Capabilities: canonicalCapabilities(request.AcceptedCapabilities), AcceptedAt: now}
		owner, ownerErr := consentPATOwner(request.WorkflowHome, consent.Capabilities)
		if ownerErr != nil {
			return result("consent_required", map[string]any{"reason": "pat_owner_binding_required", "required_capabilities": required}), nil
		}
		consent.GitHubOwner = owner
		if !sameCapabilities(consent.Capabilities, required) {
			return result("consent_required", map[string]any{"required_capabilities": required}), nil
		}
		if err := writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "consents", consent.ID+".json"), consent); err != nil {
			return Result{}, err
		}
		if freshHome && e.AfterConsentWritten != nil {
			e.AfterConsentWritten(request.WorkflowHome)
		}
	}
	// A fresh Attempt is durable immediately after its Consent and before the
	// PAT, dependency, PATH, dispatcher, or payload mutations which can fail.
	// This makes a consent-only crash window a precise forward-repair subject.
	freshCandidate := activeErr != nil && (freshHome || recoveringFresh)
	var freshAttempt Attempt
	if freshCandidate {
		if recoveringFresh {
			consent, freshAttempt = recoveryConsent, recoveryAttempt
			if freshAttempt.ID == "" {
				freshAttempt = Attempt{SchemaVersion: ProtocolVersion, ID: randomID("attempt-"), TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, Generation: generationName(request.BundleDigest), ConsentID: consent.ID, Phase: "consented", CreatedAt: now}
				if err := writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "attempts", freshAttempt.ID+".json"), freshAttempt); err != nil {
					return Result{}, err
				}
			}
		} else {
			freshAttempt = Attempt{SchemaVersion: ProtocolVersion, ID: randomID("attempt-"), TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, Generation: generationName(request.BundleDigest), ConsentID: consent.ID, Phase: "consented", CreatedAt: now}
			if err := writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "attempts", freshAttempt.ID+".json"), freshAttempt); err != nil {
				return Result{}, err
			}
		}
		if e.AfterConsentRecorded != nil {
			e.AfterConsentRecorded(request.WorkflowHome)
		}
	}
	failPreactivation := func(cause error) (Result, error) {
		if freshCandidate {
			return e.failFreshAttempt(request.WorkflowHome, freshAttempt, cause)
		}
		return result("blocked", map[string]any{"error": cause.Error()}), nil
	}
	// Consent is the first durable Home record for a fresh installation. Only
	// now create the regular in-Home lock used by the rest of the lifecycle.
	if freshHome {
		unlock, lockErr := acquireLock(request.WorkflowHome)
		if lockErr != nil {
			return blocked(lockErr), nil
		}
		defer unlock()
	}
	if oldDB != nil && e.Lifecycle != nil {
		if err := e.Lifecycle.Stop(ctx, request.WorkflowHome, active); err != nil {
			return result("blocked", map[string]any{"error": err.Error()}), nil
		}
		oldStopped = true
	}
	if request.PAT != "" {
		if _, err := canonicalPATCapability(request.WorkflowHome, consent.Capabilities); err != nil {
			return result("consent_required", map[string]any{"reason": "pat_owner_binding_required"}), nil
		}
		// Do not replace the shared credential until the supplied plaintext has
		// been authenticated against the exact consent owner. In particular an
		// upgrade failure must leave a restartable old generation with its prior
		// fingerprint and credential bytes intact.
		if err := e.verifyPATIdentity(ctx, request, consent); err != nil {
			return failPreactivation(err)
		}
		credentialPath := filepath.Join(request.WorkflowHome, "state", "credentials", "github.pat")
		if existing, err := os.ReadFile(credentialPath); err == nil {
			priorCredential, credentialWasPresent = existing, true
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		if err := writeSecret(credentialPath, request.PAT); err != nil {
			return failPreactivation(err)
		}
		credentialReplaced = true
	}
	if e.Lifecycle != nil {
		if err := e.Lifecycle.Prepare(ctx, request, consent); err != nil {
			return failPreactivation(err)
		}
	}
	// Stable Dispatcher is the exact Launcher bytes from the selected Bundle,
	// never a copied versioned CLI.
	if err := copyFile(filepath.Join(e.BundleRoot, "setup", "workflow-setup.exe"), filepath.Join(request.WorkflowHome, "bin", "workflow.exe"), 0o700); err != nil {
		return failPreactivation(err)
	}
	if e.ReconcilePath != nil {
		if err := e.ReconcilePath(filepath.Join(request.WorkflowHome, "bin")); err != nil {
			return failPreactivation(err)
		}
	}
	if resumeActiveRepair {
		return e.repairActiveGeneration(ctx, request, active, consent, verifiedRelease, now)
	}

	clearOldFence := func() {
		if oldDB != nil {
			_ = oldDB.ClearMaintenanceFence(context.Background(), operation)
		}
	}

	attempt := Attempt{SchemaVersion: ProtocolVersion, ID: randomID("attempt-"), TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, Generation: generationName(request.BundleDigest), ConsentID: consent.ID, Phase: "staged", CreatedAt: now}
	if freshCandidate {
		attempt = freshAttempt
	}
	resume := activeErr == nil && active.Readiness == activeRepairRequired && active.Version == request.TargetVersion && active.BundleDigest == request.BundleDigest
	if freshCandidate {
		// Attempt already persisted with its consent before any pre-activation work.
	} else if resume {
		if existing, err := readAttempt(request.WorkflowHome, active.AttemptID); err == nil {
			attempt = existing
			attempt.ConsentID = consent.ID
		}
	} else {
		if activeErr == nil {
			attempt.SourceAttempt = active.AttemptID
		}
		if err := writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "attempts", attempt.ID+".json"), attempt); err != nil {
			return Result{}, err
		}
	}
	generation := filepath.Join(request.WorkflowHome, "platform", "generations", attempt.Generation)
	if err := e.stageGeneration(generation); err != nil {
		if freshCandidate {
			return e.failFreshAttempt(request.WorkflowHome, attempt, err)
		}
		return e.failAttempt(request.WorkflowHome, attempt, err)
	}
	if oldDB != nil {
		if _, err := oldDB.CreateOnlineBackup(ctx, filepath.Join(request.WorkflowHome, "backups", attempt.ID+".db"), now); err != nil {
			return e.failAttempt(request.WorkflowHome, attempt, err)
		}
		// SQLite's online backup is used as the consistent copy source.
		if err := copyFile(filepath.Join(request.WorkflowHome, "backups", attempt.ID+".db"), filepath.Join(generation, "workflow.db"), 0o600); err != nil {
			return e.failAttempt(request.WorkflowHome, attempt, err)
		}
	}
	candidate, err := store.Open(ctx, filepath.Join(generation, "workflow.db"))
	if err != nil {
		if freshCandidate {
			return e.failFreshAttempt(request.WorkflowHome, attempt, err)
		}
		return e.failAttempt(request.WorkflowHome, attempt, err)
	}
	if verifiedRelease != nil {
		verifiedRelease.VerifiedAt = now
		verifiedRelease.ActivatedAt = now
		if releaseErr := candidate.ActivateWorkerRelease(ctx, *verifiedRelease); releaseErr != nil {
			_ = candidate.Close()
			if freshCandidate {
				return e.failFreshAttempt(request.WorkflowHome, attempt, releaseErr)
			}
			return e.failAttempt(request.WorkflowHome, attempt, releaseErr)
		}
	}
	if request.PAT != "" {
		target, targetErr := canonicalPATCapability(request.WorkflowHome, consent.Capabilities)
		if targetErr != nil {
			_ = candidate.Close()
			if freshCandidate {
				return e.failFreshAttempt(request.WorkflowHome, attempt, targetErr)
			}
			return e.failAttempt(request.WorkflowHome, attempt, targetErr)
		}
		var binding PATCapabilityTarget
		_ = json.Unmarshal([]byte(target), &binding)
		verify := e.VerifyPAT
		if verify == nil {
			verify = func(ctx context.Context, token, owner string) (githubcredential.Verification, error) {
				return (githubcredential.Verifier{}).Verify(ctx, token, owner)
			}
		}
		verification, verifyErr := verify(ctx, request.PAT, binding.Owner)
		if verifyErr != nil || verification.Owner != binding.Owner || credential.Fingerprint(request.PAT) != verification.FingerprintSHA256 {
			_ = candidate.Close()
			if verifyErr == nil {
				verifyErr = errors.New("GitHub PAT verification fingerprint or owner mismatch")
			}
			if freshCandidate {
				return e.failFreshAttempt(request.WorkflowHome, attempt, verifyErr)
			}
			return e.failAttempt(request.WorkflowHome, attempt, verifyErr)
		}
		if recordErr := candidate.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: verification.FingerprintSHA256, Login: verification.Login, UserID: verification.UserID, Owner: verification.Owner, Scopes: verification.Scopes, CredentialPath: binding.Path, Status: "verified", VerifiedAt: verification.VerifiedAt}); recordErr != nil {
			_ = candidate.Close()
			if freshCandidate {
				return e.failFreshAttempt(request.WorkflowHome, attempt, recordErr)
			}
			return e.failAttempt(request.WorkflowHome, attempt, recordErr)
		}
	}
	_ = candidate.Close()
	attempt.Phase = "verified"
	if err := writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "attempts", attempt.ID+".json"), attempt); err != nil {
		if freshCandidate {
			return e.failFreshAttempt(request.WorkflowHome, attempt, err)
		}
		return Result{}, err
	}
	active = Active{SchemaVersion: ProtocolVersion, Generation: attempt.Generation, Version: request.TargetVersion, BundleDigest: request.BundleDigest, AttemptID: attempt.ID, ConsentID: consent.ID, Readiness: activeRepairRequired, ActivatedAt: now}
	if err := writeJSONAtomic(activePath(request.WorkflowHome), active); err != nil {
		if freshCandidate {
			return e.failFreshAttempt(request.WorkflowHome, attempt, err)
		}
		return Result{}, err
	}
	activationCommitted = true
	// The active record is now the sole authority. Failure from this point is
	// deliberately returned as repair_required and never switches backwards.
	if err := e.verifyGeneration(request.WorkflowHome, active); err != nil {
		return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
	}
	if e.Lifecycle != nil {
		if err := e.Lifecycle.Start(ctx, request.WorkflowHome, active); err != nil {
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
		if err := e.Lifecycle.Ready(ctx, request.WorkflowHome, active); err != nil {
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
	}
	if candidate, err := store.Open(ctx, filepath.Join(request.WorkflowHome, "platform", "generations", active.Generation, "workflow.db")); err == nil {
		_ = candidate.ClearMaintenanceFence(ctx, operation)
		_ = candidate.Close()
	}
	active.Readiness = activeReady
	if err := writeJSONAtomic(activePath(request.WorkflowHome), active); err != nil {
		return Result{}, err
	}
	attempt.Phase = "ready"
	_ = writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "attempts", attempt.ID+".json"), attempt)
	clearOldFence()
	return result("ready", map[string]any{"active": active}), nil
}

// repairActiveGeneration is deliberately separate from staging.  Once
// active.json names a repair_required candidate, that generation and its
// SQLite file are the durable forward-repair subject.  Copying the active
// database back over itself is both non-idempotent and unsafe on Windows.
func (e Engine) repairActiveGeneration(ctx context.Context, request Request, active Active, consent Consent, verifiedRelease *store.WorkerRelease, now time.Time) (Result, error) {
	attempt, err := readAttempt(request.WorkflowHome, active.AttemptID)
	if err != nil {
		return result("repair_required", map[string]any{"active": active, "error": "active repair Attempt is unavailable"}), nil
	}
	if attempt.Generation != active.Generation || attempt.TargetVersion != active.Version || attempt.BundleDigest != active.BundleDigest || attempt.ConsentID != consent.ID {
		return result("consent_required", map[string]any{"reason": "active repair identity differs from retained consent"}), nil
	}
	if err := e.verifyGeneration(request.WorkflowHome, active); err != nil {
		return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
	}
	candidate, err := store.Open(ctx, filepath.Join(request.WorkflowHome, "platform", "generations", active.Generation, "workflow.db"))
	if err != nil {
		return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
	}
	if verifiedRelease != nil {
		verifiedRelease.VerifiedAt = now
		verifiedRelease.ActivatedAt = now
		if err := candidate.ActivateWorkerRelease(ctx, *verifiedRelease); err != nil {
			_ = candidate.Close()
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
	}
	if request.PAT != "" {
		if err := e.verifyAndRecordPAT(ctx, candidate, request, consent); err != nil {
			_ = candidate.Close()
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
	}
	if err := candidate.Close(); err != nil {
		return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
	}
	if e.Lifecycle != nil {
		if err := e.Lifecycle.Start(ctx, request.WorkflowHome, active); err != nil {
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
		if err := e.Lifecycle.Ready(ctx, request.WorkflowHome, active); err != nil {
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
	}
	if candidate, err := store.Open(ctx, filepath.Join(request.WorkflowHome, "platform", "generations", active.Generation, "workflow.db")); err == nil {
		_ = candidate.ClearMaintenanceFenceForBundle(ctx, active.BundleDigest)
		_ = candidate.Close()
	}
	active.Readiness = activeReady
	if err := writeJSONAtomic(activePath(request.WorkflowHome), active); err != nil {
		return Result{}, err
	}
	attempt.Phase = "ready"
	_ = writeJSONAtomic(filepath.Join(request.WorkflowHome, "platform", "attempts", attempt.ID+".json"), attempt)
	return result("ready", map[string]any{"active": active}), nil
}

func (e Engine) Verify(ctx context.Context, request Request) (Result, error) {
	_ = ctx
	active, err := ReadActive(request.WorkflowHome)
	if err != nil {
		return blocked(err), nil
	}
	if active.Readiness != activeReady {
		return result("repair_required", map[string]any{"active": active}), nil
	}
	if err := e.verifyGeneration(request.WorkflowHome, active); err != nil {
		return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
	}
	if e.Lifecycle != nil {
		if err := e.Lifecycle.Ready(ctx, request.WorkflowHome, active); err != nil {
			return result("repair_required", map[string]any{"active": active, "error": err.Error()}), nil
		}
	}
	return result("platform_ready", map[string]any{"active": active}), nil
}

func (e Engine) stageGeneration(generation string) error {
	if e.BundleRoot == "" {
		return errors.New("launcher bundle root is required")
	}
	if _, err := os.Stat(generation); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, pair := range [][2]string{{"platform-release.json", "platform-release.json"}, {"platform/workflow.exe", "workflow.exe"}, {"setup/workflow-setup.exe", "workflow-setup.exe"}} {
		if err := copyFile(filepath.Join(e.BundleRoot, filepath.FromSlash(pair[0])), filepath.Join(generation, pair[1]), 0o700); err != nil {
			return fmt.Errorf("stage %s: %w", pair[0], err)
		}
	}
	for _, name := range []string{"skills", "repository-contract"} {
		if err := copyTree(filepath.Join(e.BundleRoot, name), filepath.Join(generation, name)); err != nil {
			return err
		}
	}
	return nil
}

func (e Engine) verifyGeneration(home string, active Active) error {
	base := filepath.Join(home, "platform", "generations", active.Generation)
	rawManifest, err := os.ReadFile(filepath.Join(base, "platform-release.json"))
	if err != nil {
		return fmt.Errorf("active generation lacks platform-release.json: %w", err)
	}
	var manifest platformrelease.BundleManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return fmt.Errorf("decode active generation manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("validate active generation manifest: %w", err)
	}
	if manifest.Version != active.Version {
		return errors.New("active generation manifest version differs from active record")
	}
	for _, file := range manifest.Files {
		name := file.Path
		switch name {
		case "platform/workflow.exe":
			name = "workflow.exe"
		case "setup/workflow-setup.exe":
			name = "workflow-setup.exe"
		}
		data, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("active generation lacks %s: %w", file.Path, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return fmt.Errorf("active generation digest differs for %s", file.Path)
		}
	}
	for _, name := range []string{"workflow.exe", "workflow-setup.exe", "workflow.db"} {
		info, err := os.Stat(filepath.Join(base, name))
		if err != nil || info.IsDir() {
			return fmt.Errorf("active generation lacks %s", name)
		}
	}
	return nil
}

func (e Engine) failAttempt(home string, attempt Attempt, cause error) (Result, error) {
	attempt.Phase = "failed"
	attempt.Diagnostics = cause.Error()
	_ = writeJSONAtomic(filepath.Join(home, "platform", "attempts", attempt.ID+".json"), attempt)
	return result("blocked", map[string]any{"error": cause.Error(), "attempt_id": attempt.ID}), nil
}

// failFreshAttempt records a failed pre-activation transition without
// inventing active.json. Its exact Consent/Attempt is the only authority a
// caller may use to repair this fresh Home.
func (e Engine) failFreshAttempt(home string, attempt Attempt, cause error) (Result, error) {
	// Only immutable Bundle copies and the stable dispatcher are discarded.
	// Consent and Attempt remain the exact recovery authority; credential bytes
	// are restored by Apply's pre-activation defer before inspect can reuse it.
	if err := os.RemoveAll(filepath.Join(home, "platform", "generations", generationName(attempt.BundleDigest))); err != nil {
		return result("blocked", map[string]any{"error": fmt.Sprintf("fresh recovery cleanup generation: %v", err), "attempt": attempt, "consent_id": attempt.ConsentID}), nil
	}
	for _, path := range []string{filepath.Join(home, "platform", "generations"), filepath.Join(home, "bin", "workflow.exe"), filepath.Join(home, "bin")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result("blocked", map[string]any{"error": fmt.Sprintf("fresh recovery cleanup artifact: %v", err), "attempt": attempt, "consent_id": attempt.ConsentID}), nil
		}
	}
	attempt.Phase = "failed"
	attempt.Diagnostics = cause.Error()
	if err := writeJSONAtomic(filepath.Join(home, "platform", "attempts", attempt.ID+".json"), attempt); err != nil {
		return Result{}, err
	}
	return result("repair_required", map[string]any{"attempt": attempt, "consent_id": attempt.ConsentID, "error": cause.Error()}), nil
}
func (e Engine) requiredCapabilities(ctx context.Context, request Request, consent *Consent) ([]Capability, error) {
	owner := strings.ToLower(strings.TrimSpace(request.GitHubOwner))
	if owner == "" && consent != nil {
		owner = strings.ToLower(strings.TrimSpace(consent.GitHubOwner))
		if owner == "" {
			var err error
			owner, err = consentPATOwner(request.WorkflowHome, consent.Capabilities)
			if err != nil {
				return nil, err
			}
		}
	}
	if owner == "" {
		if active, err := ReadActive(request.WorkflowHome); err == nil {
			if activeConsent, consentErr := readConsent(request.WorkflowHome, active.ConsentID); consentErr == nil {
				return e.requiredCapabilities(ctx, request, &activeConsent)
			}
		}
	}
	if owner == "" || strings.ContainsAny(owner, "/\\\t\r\n ") {
		return nil, errors.New("normalized GitHub owner is required")
	}
	raw, err := json.Marshal(PATCapabilityTarget{Path: filepath.Clean(filepath.Join(request.WorkflowHome, "state", "credentials", "github.pat")), Owner: owner})
	if err != nil {
		return nil, err
	}
	compatibility, err := e.bundleCompatibility()
	if err != nil {
		return nil, err
	}
	docker, err := e.dockerConsentTarget(ctx, compatibility)
	if err != nil {
		return nil, err
	}
	dockerRaw, err := json.Marshal(docker)
	if err != nil {
		return nil, err
	}
	workerRaw, err := json.Marshal(WorkerImageCapabilityTarget{Image: compatibility.WorkerImage})
	if err != nil {
		return nil, err
	}
	return []Capability{{"install_platform", request.TargetVersion + " " + request.BundleDigest + " " + request.WorkflowHome}, {"modify_user_path", filepath.Join(request.WorkflowHome, "bin")}, {"persist_plaintext_pat", string(raw)}, {"manage_docker_desktop", string(dockerRaw)}, {"pull_worker_image", string(workerRaw)}, {"start_control_plane", request.WorkflowHome + " " + generationName(request.BundleDigest)}}, nil
}

func consentPATOwner(home string, values []Capability) (string, error) {
	raw, err := canonicalPATCapability(home, values)
	if err != nil {
		return "", err
	}
	var target PATCapabilityTarget
	if err := json.Unmarshal([]byte(raw), &target); err != nil {
		return "", err
	}
	return target.Owner, nil
}

func (e Engine) verifyAndRecordPAT(ctx context.Context, candidate *store.Store, request Request, consent Consent) error {
	target, err := canonicalPATCapability(request.WorkflowHome, consent.Capabilities)
	if err != nil {
		return err
	}
	var binding PATCapabilityTarget
	if err := json.Unmarshal([]byte(target), &binding); err != nil {
		return err
	}
	verify := e.VerifyPAT
	if verify == nil {
		verify = func(ctx context.Context, token, owner string) (githubcredential.Verification, error) {
			return (githubcredential.Verifier{}).Verify(ctx, token, owner)
		}
	}
	verification, err := verify(ctx, request.PAT, binding.Owner)
	if err != nil {
		return err
	}
	if verification.Owner != binding.Owner || credential.Fingerprint(request.PAT) != verification.FingerprintSHA256 {
		return errors.New("GitHub PAT verification fingerprint or owner mismatch")
	}
	return candidate.RecordGitHubPATVerification(ctx, store.GitHubPATVerification{FingerprintSHA256: verification.FingerprintSHA256, Login: verification.Login, UserID: verification.UserID, Owner: verification.Owner, Scopes: verification.Scopes, CredentialPath: binding.Path, Status: "verified", VerifiedAt: verification.VerifiedAt})
}

func (e Engine) verifyPATIdentity(ctx context.Context, request Request, consent Consent) error {
	target, err := canonicalPATCapability(request.WorkflowHome, consent.Capabilities)
	if err != nil {
		return err
	}
	var binding PATCapabilityTarget
	if err := json.Unmarshal([]byte(target), &binding); err != nil {
		return err
	}
	verify := e.VerifyPAT
	if verify == nil {
		verify = func(ctx context.Context, token, owner string) (githubcredential.Verification, error) {
			return (githubcredential.Verifier{}).Verify(ctx, token, owner)
		}
	}
	verification, err := verify(ctx, request.PAT, binding.Owner)
	if err != nil {
		return err
	}
	if verification.Owner != binding.Owner || credential.Fingerprint(request.PAT) != verification.FingerprintSHA256 {
		return errors.New("GitHub PAT verification fingerprint or owner mismatch")
	}
	return nil
}

func canonicalPATCapability(home string, values []Capability) (string, error) {
	want := filepath.Clean(filepath.Join(home, "state", "credentials", "github.pat"))
	for _, value := range values {
		if value.Name == "persist_plaintext_pat" {
			var target PATCapabilityTarget
			if json.Unmarshal([]byte(value.Value), &target) != nil || filepath.Clean(target.Path) != want || !filepath.IsAbs(target.Path) || strings.TrimSpace(target.Owner) == "" {
				return "", errors.New("persist_plaintext_pat requires canonical absolute path and bound owner")
			}
			target.Path, target.Owner = want, strings.ToLower(strings.TrimSpace(target.Owner))
			raw, _ := json.Marshal(target)
			return string(raw), nil
		}
	}
	return "", errors.New("persist_plaintext_pat owner binding is required")
}
func ReadActive(home string) (Active, error) {
	var active Active
	raw, err := os.ReadFile(activePath(home))
	if err != nil {
		return active, err
	}
	if err := json.Unmarshal(raw, &active); err != nil {
		return active, err
	}
	if active.SchemaVersion != ProtocolVersion || active.Generation == "" || (active.Readiness != activeReady && active.Readiness != activeRepairRequired) {
		return active, errors.New("active.json is invalid")
	}
	return active, nil
}
func activePath(home string) string { return filepath.Join(home, "platform", "active.json") }

func validateHomeForApply(home string) error {
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err := os.Stat(activePath(home)); err == nil {
		return nil
	}
	return errors.New("nonempty Workflow Home is not a generation-based installation")
}

func validateApplyRequest(request Request) error {
	if strings.TrimSpace(request.WorkflowHome) == "" || !filepath.IsAbs(request.WorkflowHome) {
		return errors.New("workflow_home must be an absolute path")
	}
	if err := validTarget(request); err != nil {
		return err
	}
	if (strings.TrimSpace(request.ConsentID) == "") == (len(request.AcceptedCapabilities) == 0) {
		return errors.New("apply requires exactly one consent_id or accepted_capabilities")
	}
	return nil
}

// preflightConsent has no write side effects. It is intentionally invoked
// before a missing Home is created and again while holding the applicable
// installation lock so that accepted capabilities cannot become stale between
// inspect and apply.
func (e Engine) preflightConsent(ctx context.Context, request Request) (Result, bool) {
	if request.ConsentID != "" {
		consent, err := readConsent(request.WorkflowHome, request.ConsentID)
		if err != nil {
			return result("consent_required", map[string]any{"reason": "consent_not_found"}), true
		}
		required, err := e.requiredCapabilities(ctx, request, &consent)
		if err != nil {
			return result("consent_required", map[string]any{"reason": "github_owner_required"}), true
		}
		if consent.TargetVersion != request.TargetVersion || consent.BundleDigest != request.BundleDigest || !e.sameRecoveryCapabilities(consent.Capabilities, required) {
			return result("consent_required", map[string]any{"reason": "consent_target_changed", "required_capabilities": required}), true
		}
		return Result{}, false
	}
	required, err := e.requiredCapabilities(ctx, request, nil)
	if err != nil {
		return result("consent_required", map[string]any{"reason": "github_owner_required"}), true
	}
	if _, err := consentPATOwner(request.WorkflowHome, canonicalCapabilities(request.AcceptedCapabilities)); err != nil {
		return result("consent_required", map[string]any{"reason": "pat_owner_binding_required", "required_capabilities": required}), true
	}
	if !sameCapabilities(canonicalCapabilities(request.AcceptedCapabilities), required) {
		return result("consent_required", map[string]any{"required_capabilities": required}), true
	}
	return Result{}, false
}

func workflowHomeFresh(home string) (bool, error) {
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}
func result(status string, evidence map[string]any) Result {
	return Result{SchemaVersion: ProtocolVersion, Status: status, Evidence: evidence}
}
func blocked(err error) Result { return result("blocked", map[string]any{"error": err.Error()}) }
func generationName(digest string) string {
	return strings.TrimPrefix(strings.ToLower(digest), "sha256:")
}
func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(time.Now().String()))
		return prefix + hex.EncodeToString(sum[:12])
	}
	return prefix + hex.EncodeToString(b)
}
func writeJSONAtomic(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'), 0o600)
}
func writeSecret(path, secret string) error { return writeAtomic(path, []byte(secret+"\n"), 0o600) }
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".workflow-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
func copyFile(from, to string, mode os.FileMode) error {
	input, err := os.Open(from)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, input)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(to, relative), 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("bundle payload must contain regular files")
		}
		return copyFile(path, filepath.Join(to, relative), 0o600)
	})
}
func acquireLock(home string) (func(), error) {
	path := filepath.Join(home, "platform", "installation.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("workflow home installation is locked: %w", err)
	}
	return func() { _ = f.Close(); _ = os.Remove(path) }, nil
}

// acquireProspectiveLock serializes first installers without writing inside a
// not-yet-created Workflow Home. The canonicalized, case-folded path is
// hashed because Windows installation paths are case-insensitive and may not
// be safe as a lock filename. The lock is process-shared through the system
// temp directory and is removed when the owning apply returns.
func acquireProspectiveLock(home string) (func(), error) {
	canonical, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	canonical = strings.ToLower(filepath.Clean(canonical))
	sum := sha256.Sum256([]byte(canonical))
	directory := filepath.Join(os.TempDir(), "agent-workflow-install-locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, hex.EncodeToString(sum[:])+".lock")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("workflow home first installation is locked: %w", err)
	}
	return func() { _ = f.Close(); _ = os.Remove(path) }, nil
}
func readConsent(home, id string) (Consent, error) {
	var c Consent
	raw, err := os.ReadFile(filepath.Join(home, "platform", "consents", id+".json"))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c)
	return c, err
}
func reusableConsent(home, version, digest string, required []Capability) (Consent, bool) {
	active, err := ReadActive(home)
	if err != nil {
		return Consent{}, false
	}
	c, err := readConsent(home, active.ConsentID)
	return c, err == nil && c.TargetVersion == version && c.BundleDigest == digest && sameCapabilities(c.Capabilities, required)
}

func (e Engine) reusableFreshRecovery(home, version, digest string, required []Capability) (Consent, Attempt, bool) {
	entries, err := os.ReadDir(filepath.Join(home, "platform", "attempts"))
	if errors.Is(err, os.ErrNotExist) || len(entries) == 0 {
		consents, consentErr := os.ReadDir(filepath.Join(home, "platform", "consents"))
		if consentErr != nil || len(consents) != 1 || consents[0].IsDir() {
			return Consent{}, Attempt{}, false
		}
		id := strings.TrimSuffix(consents[0].Name(), ".json")
		if !safeConsentID(id) || !regularFile(filepath.Join(home, "platform", "consents", consents[0].Name())) {
			return Consent{}, Attempt{}, false
		}
		consent, readErr := readConsent(home, id)
		if readErr != nil || consent.SchemaVersion != ProtocolVersion || consent.ID != id || consent.TargetVersion != version || consent.BundleDigest != digest || filepath.Clean(consent.WorkflowHome) != filepath.Clean(home) || !e.sameRecoveryCapabilities(consent.Capabilities, required) || !consentOnlyLayout(home, consent) {
			return Consent{}, Attempt{}, false
		}
		return consent, Attempt{}, true
	}
	if err != nil || len(entries) != 1 || entries[0].IsDir() || !strings.HasSuffix(entries[0].Name(), ".json") {
		return Consent{}, Attempt{}, false
	}
	filenameID := strings.TrimSuffix(entries[0].Name(), ".json")
	if !safeAttemptID(filenameID) {
		return Consent{}, Attempt{}, false
	}
	attempt, err := readAttempt(home, filenameID)
	if err != nil {
		return Consent{}, Attempt{}, false
	}
	if !safeConsentID(attempt.ConsentID) || !regularFile(filepath.Join(home, "platform", "consents", attempt.ConsentID+".json")) {
		return Consent{}, Attempt{}, false
	}
	consent, err := readConsent(home, attempt.ConsentID)
	if err != nil || !e.validFreshRecovery(home, filenameID, attempt, consent, version, digest, required) {
		return Consent{}, Attempt{}, false
	}
	if _, err := ReadActive(home); err == nil {
		return Consent{}, Attempt{}, false
	}
	if !freshRecoveryLayout(home, consent, attempt) {
		return Consent{}, Attempt{}, false
	}
	return consent, attempt, true
}

func (e Engine) freshRecoveryForApply(ctx context.Context, request Request) (Consent, Attempt, bool, error) {
	home := request.WorkflowHome
	if request.ConsentID == "" {
		return Consent{}, Attempt{}, false, nil
	}
	if !safeConsentID(request.ConsentID) || !regularFile(filepath.Join(home, "platform", "consents", request.ConsentID+".json")) {
		return Consent{}, Attempt{}, false, nil
	}
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return Consent{}, Attempt{}, false, nil
	}
	if err != nil {
		return Consent{}, Attempt{}, false, err
	}
	if len(entries) == 0 {
		return Consent{}, Attempt{}, false, nil
	}
	consent, err := readConsent(home, request.ConsentID)
	if err != nil {
		return Consent{}, Attempt{}, false, nil
	}
	entries, err = os.ReadDir(filepath.Join(home, "platform", "attempts"))
	if errors.Is(err, os.ErrNotExist) || len(entries) == 0 {
		required, requiredErr := e.requiredCapabilities(ctx, request, &consent)
		if requiredErr == nil && consent.SchemaVersion == ProtocolVersion && consent.ID == request.ConsentID && consent.TargetVersion == request.TargetVersion && consent.BundleDigest == request.BundleDigest && e.sameRecoveryCapabilities(consent.Capabilities, required) && consentOnlyLayout(home, consent) {
			return consent, Attempt{}, true, nil
		}
		return Consent{}, Attempt{}, false, nil
	}
	if err != nil || len(entries) != 1 || entries[0].IsDir() {
		return Consent{}, Attempt{}, false, nil
	}
	filenameID := strings.TrimSuffix(entries[0].Name(), ".json")
	if !safeAttemptID(filenameID) || !regularFile(filepath.Join(home, "platform", "attempts", filenameID+".json")) {
		return Consent{}, Attempt{}, false, nil
	}
	attempt, err := readAttempt(home, filenameID)
	if err != nil {
		return Consent{}, Attempt{}, false, nil
	}
	required, requiredErr := e.requiredCapabilities(ctx, request, &consent)
	if requiredErr != nil || !e.validFreshRecovery(home, filenameID, attempt, consent, request.TargetVersion, request.BundleDigest, required) {
		return Consent{}, Attempt{}, false, nil
	}
	return consent, attempt, true, nil
}

func consentOnlyLayout(home string, consent Consent) bool {
	entries, err := os.ReadDir(home)
	if err != nil {
		return false
	}
	if len(entries) < 1 || len(entries) > 2 {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "state" {
			if !consentOnlyStateLayout(filepath.Join(home, "state")) {
				return false
			}
			continue
		}
		if entry.Name() != "platform" || !entry.IsDir() {
			return false
		}
	}
	platform, err := os.ReadDir(filepath.Join(home, "platform"))
	if err != nil || len(platform) < 1 || len(platform) > 2 {
		return false
	}
	for _, entry := range platform {
		if entry.Name() == "consents" && entry.IsDir() {
			continue
		}
		if entry.Name() == "installation.lock" && regularFile(filepath.Join(home, "platform", entry.Name())) {
			continue
		}
		return false
	}
	consents, err := os.ReadDir(filepath.Join(home, "platform", "consents"))
	if err != nil || len(consents) != 1 || consents[0].Name() != consent.ID+".json" || !regularFile(filepath.Join(home, "platform", "consents", consents[0].Name())) {
		return false
	}
	return true
}

// consentOnlyStateLayout accepts only state which may have existed before the
// Consent was durably written. It must never follow a symlink or Windows
// reparse point: later credential writes must remain inside Workflow Home.
func consentOnlyStateLayout(statePath string) bool {
	info, err := os.Lstat(statePath)
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return false
	}
	entries, err := os.ReadDir(statePath)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "setup.lock":
			if !regularFile(filepath.Join(statePath, entry.Name())) {
				return false
			}
		case "credentials":
			credentialsPath := filepath.Join(statePath, entry.Name())
			credentials, credentialsErr := os.Lstat(credentialsPath)
			if credentialsErr != nil || !credentials.IsDir() || credentials.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return false
			}
			contents, contentsErr := os.ReadDir(credentialsPath)
			if contentsErr != nil || len(contents) != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func safeConsentID(id string) bool { return safeRecordID(id, "consent-") }
func safeAttemptID(id string) bool { return safeRecordID(id, "attempt-") }
func safeRecordID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+24 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func (e Engine) validFreshRecovery(home, filenameID string, attempt Attempt, consent Consent, version, digest string, required []Capability) bool {
	if filenameID == "" || strings.ContainsAny(filenameID, `/\\`) || attempt.SchemaVersion != ProtocolVersion || attempt.ID != filenameID || attempt.TargetVersion != version || attempt.BundleDigest != digest || attempt.Generation != generationName(digest) || attempt.ConsentID == "" || attempt.CreatedAt.IsZero() {
		return false
	}
	switch attempt.Phase {
	case "consented", "staged", "verified", "failed":
	default:
		return false
	}
	if consent.SchemaVersion != ProtocolVersion || consent.ID != attempt.ConsentID || !safeConsentID(consent.ID) || consent.TargetVersion != version || consent.BundleDigest != digest || !filepath.IsAbs(consent.WorkflowHome) || filepath.Clean(consent.WorkflowHome) != filepath.Clean(home) || !e.sameRecoveryCapabilities(consent.Capabilities, required) {
		return false
	}
	return freshRecoveryLayout(home, consent, attempt)
}

// sameRecoveryCapabilities permits only the monotonic Docker transition an
// accepted install/upgrade itself causes: after the approved Desktop version
// is installed, inspect observes exact reuse. Every other capability remains
// byte-for-byte consent-bound.
func (e Engine) sameRecoveryCapabilities(accepted, required []Capability) bool {
	if sameCapabilities(accepted, required) {
		return true
	}
	accepted = canonicalCapabilities(accepted)
	required = canonicalCapabilities(required)
	if len(accepted) != len(required) {
		return false
	}
	for i := range accepted {
		if accepted[i].Name != required[i].Name {
			return false
		}
		if accepted[i].Name != "manage_docker_desktop" {
			if accepted[i].Value != required[i].Value {
				return false
			}
			continue
		}
		oldTarget, oldErr := dockerCapability([]Capability{accepted[i]})
		newTarget, newErr := dockerCapability([]Capability{required[i]})
		compatibility, compatibilityErr := e.bundleCompatibility()
		if oldErr != nil || newErr != nil || compatibilityErr != nil || validateDockerCapability(oldTarget, compatibility) != nil || (oldTarget.Action != dockerActionInstall && oldTarget.Action != dockerActionUpgrade) || newTarget.Action != dockerActionReuse || oldTarget.RequiredVersion != newTarget.RequiredVersion || newTarget.ObservedVersion != newTarget.RequiredVersion {
			return false
		}
	}
	return true
}

func freshRecoveryLayout(home string, consent Consent, attempt Attempt) bool {
	directories := map[string]bool{"platform": true, "platform/consents": true, "platform/attempts": true, "state": true, "state/credentials": true}
	requiredDirectories := map[string]bool{"platform": true, "platform/consents": true, "platform/attempts": true}
	files := map[string]bool{"platform/consents/" + consent.ID + ".json": true, "platform/attempts/" + attempt.ID + ".json": true, "platform/installation.lock": true, "state/credentials/github.pat": true, "state/setup.lock": true}
	seen := map[string]bool{}
	valid := true
	_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			valid = false
			return err
		}
		rel, relErr := filepath.Rel(home, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			valid = false
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			valid = false
			return nil
		}
		if directories[rel] {
			if !info.IsDir() {
				valid = false
			} else {
				seen[rel] = true
			}
			return nil
		}
		if files[rel] {
			if !info.Mode().IsRegular() {
				valid = false
			} else {
				seen[rel] = true
			}
			return nil
		}
		valid = false
		return nil
	})
	for required := range requiredDirectories {
		if !seen[required] {
			valid = false
		}
	}
	for required := range files {
		if required != "state/credentials/github.pat" && required != "state/setup.lock" && required != "platform/installation.lock" && !seen[required] {
			valid = false
		}
	}
	return valid
}

func readAttempt(home, id string) (Attempt, error) {
	var attempt Attempt
	raw, err := os.ReadFile(filepath.Join(home, "platform", "attempts", id+".json"))
	if err != nil {
		return attempt, err
	}
	err = json.Unmarshal(raw, &attempt)
	return attempt, err
}
func canonicalCapabilities(values []Capability) []Capability {
	out := append([]Capability(nil), values...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func sameCapabilities(a, b []Capability) bool {
	a, b = canonicalCapabilities(a), canonicalCapabilities(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func activeRuns(database string) (int, []string, error) {
	s, err := store.OpenReadOnly(context.Background(), database)
	if err != nil {
		return 0, nil, err
	}
	defer s.Close()
	return s.ActiveWorkerRuns(context.Background(), time.Now().UTC())
}
