package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSetupPlansAreImmutableAndResultsAppend(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	plan := SetupPlanRecord{PlanID: "plan-1", Kind: "platform_bootstrap", SchemaVersion: 1, Target: `C:\repo`, DigestSHA256: repeatHex('a'), CanonicalJSON: `{"plan_id":"plan-1"}`, Projection: "Bootstrap", CreatedAt: now}
	if err := db.RecordSetupPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSetupPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	changed := plan
	changed.Projection = "changed"
	if err := db.RecordSetupPlan(ctx, changed); !errors.Is(err, ErrSetupPlanConflict) {
		t.Fatalf("conflict = %v", err)
	}
	for attempt, status := range []string{"incomplete", "succeeded"} {
		result := SetupExecutionResult{PlanID: plan.PlanID, Attempt: attempt + 1, Status: status, EffectsJSON: "[]", StartedAt: now, CompletedAt: now.Add(time.Second)}
		if err := db.AppendSetupExecutionResult(ctx, result); err != nil {
			t.Fatal(err)
		}
	}
	results, err := db.SetupExecutionResults(ctx, plan.PlanID)
	if err != nil || len(results) != 2 || results[1].Status != "succeeded" {
		t.Fatalf("results = %#v, %v", results, err)
	}
}

func TestLatestSetupPlanSelectsKindAndNewestCreation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	for _, plan := range []SetupPlanRecord{
		{PlanID: "platform-old", Kind: "platform_bootstrap", SchemaVersion: 1, Target: `C:\home`, DigestSHA256: repeatHex('1'), CanonicalJSON: `{}`, Projection: "old", CreatedAt: now},
		{PlanID: "repo", Kind: "repository_onboarding", SchemaVersion: 1, Target: `C:\repo`, DigestSHA256: repeatHex('2'), CanonicalJSON: `{}`, Projection: "repo", CreatedAt: now.Add(time.Second)},
		{PlanID: "platform-new", Kind: "platform_bootstrap", SchemaVersion: 1, Target: `C:\home`, DigestSHA256: repeatHex('3'), CanonicalJSON: `{}`, Projection: "new", CreatedAt: now.Add(2 * time.Second)},
	} {
		if err := db.RecordSetupPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.LatestSetupPlan(ctx, "platform_bootstrap")
	if err != nil || got.PlanID != "platform-new" {
		t.Fatalf("latest = %#v, %v", got, err)
	}
}

func TestPATVerificationAndAdmissionsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	now := time.Now().UTC()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	verification := GitHubPATVerification{FingerprintSHA256: repeatHex('b'), Login: "user", UserID: 42, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: `C:\home\github.pat`, Status: "verified", VerifiedAt: now}
	if err := db.RecordGitHubPATVerification(ctx, verification); err != nil {
		t.Fatal(err)
	}
	admission := RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: repeatHex('c'), ContractVersion: "1", ManifestDigestSHA256: repeatHex('d'), Eligible: true, VerifiedAt: now}
	if err := db.RecordRepositoryAdmission(ctx, admission); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GitHubPATVerification(ctx)
	if err != nil || got.Login != "user" || len(got.Scopes) != 2 {
		t.Fatalf("verification = %#v, %v", got, err)
	}
	gotAdmission, err := db.RepositoryAdmission(ctx, "owner/repo")
	if err != nil || !gotAdmission.Eligible {
		t.Fatalf("admission = %#v, %v", gotAdmission, err)
	}
}

func TestPlatformAndRuntimeObservationReadBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	bundleJSON := `[{"path":"bin/workflow.exe","sha256":"` + repeatHex('d') + `"}]`
	bundleSum := sha256.Sum256([]byte(bundleJSON))
	platform := PlatformInstallation{PlatformVersion: "1.2.3", ReleaseManifestDigestSHA256: repeatHex('e'), PlatformSetupContractDigestSHA256: repeatHex('c'), WorkflowCLISHA256: repeatHex('d'), ReleaseBundledFilesJSON: bundleJSON, ReleaseBundledFilesDigestSHA256: hex.EncodeToString(bundleSum[:]), WorkflowHome: `C:\Workflow`, InstalledAt: now, VerifiedAt: now}
	if err := db.RecordPlatformInstallation(ctx, platform); err != nil {
		t.Fatal(err)
	}
	got, err := db.PlatformInstallation(ctx)
	if err != nil || got.PlatformVersion != "1.2.3" || got.PlatformSetupContractDigestSHA256 != repeatHex('c') || got.WorkflowCLISHA256 != repeatHex('d') || got.ReleaseBundledFilesJSON != bundleJSON || got.ReleaseBundledFilesDigestSHA256 != platform.ReleaseBundledFilesDigestSHA256 {
		t.Fatalf("platform = %#v, %v", got, err)
	}
	if err := db.AuthorizeControlPlane(ctx, platform, repeatHex('a')); err != nil {
		t.Fatal(err)
	}
	got, err = db.PlatformInstallation(ctx)
	if err != nil || got.ControlPlanePlanDigestSHA256 != repeatHex('a') {
		t.Fatalf("authorized platform = %#v, %v", got, err)
	}
	drifted := platform
	drifted.WorkflowCLISHA256 = repeatHex('f')
	if err := db.AuthorizeControlPlane(ctx, drifted, repeatHex('b')); err == nil {
		t.Fatal("authorized Control Plane against drifted durable pins")
	}
	runtime := ControlPlaneRuntimeObservation{PID: 123, ProcessStartedAt: now, EndpointsJSON: `{"health":"http://127.0.0.1:8080/health"}`, PlatformVersion: "1.2.3", PlanDigestSHA256: repeatHex('f'), ObservedAt: now}
	if err := db.RecordControlPlaneRuntimeObservation(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	runtimeGot, err := db.ControlPlaneRuntimeObservation(ctx)
	if err != nil || runtimeGot.PID != 123 {
		t.Fatalf("runtime = %#v, %v", runtimeGot, err)
	}
}

func repeatHex(value byte) string {
	result := make([]byte, 64)
	for i := range result {
		result[i] = value
	}
	return string(result)
}
