package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOnboardingPlansRemainImmutableAndExecutionResultsAppend(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	plan := SetupPlanRecord{PlanID: "onboard-1", Kind: "repository_onboarding", SchemaVersion: 1, Target: `C:\repo`, DigestSHA256: repeatHex('a'), CanonicalJSON: `{"plan_id":"onboard-1"}`, Projection: "owner/repo", CreatedAt: now}
	if err := db.RecordSetupPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	changed := plan
	changed.Projection = "owner/other"
	if err := db.RecordSetupPlan(ctx, changed); !errors.Is(err, ErrSetupPlanConflict) {
		t.Fatalf("conflict = %v", err)
	}
	for attempt, status := range []string{"incomplete", "succeeded"} {
		if err := db.AppendSetupExecutionResult(ctx, SetupExecutionResult{PlanID: plan.PlanID, Attempt: attempt + 1, Status: status, EffectsJSON: "[]", StartedAt: now, CompletedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := db.SetupExecutionResults(ctx, plan.PlanID)
	if err != nil || len(results) != 2 || results[1].Status != "succeeded" {
		t.Fatalf("results = %#v, %v", results, err)
	}
}

func TestRepositoryCreateAttemptEvidenceRemainsPlanBound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	plan := SetupPlanRecord{PlanID: "onboard-2", Kind: "repository_onboarding", SchemaVersion: 1, Target: `C:\repo`, DigestSHA256: repeatHex('a'), CanonicalJSON: `{}`, Projection: "owner/repo", CreatedAt: now}
	if err := db.RecordSetupPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	event := SetupRepositoryCreateAttemptEvent{PlanID: plan.PlanID, PlanDigestSHA256: plan.DigestSHA256, EffectID: "create-repository", ExecutionAttempt: 1, Event: RepositoryCreateStarted, Owner: "owner", Name: "repo", Private: true, ApprovalAbsentRepository: "owner/repo", RecordedAt: now}
	if err := db.AppendSetupRepositoryCreateAttemptEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	event.PlanDigestSHA256 = repeatHex('b')
	if err := db.AppendSetupRepositoryCreateAttemptEvent(ctx, event); !errors.Is(err, ErrSetupPlanConflict) {
		t.Fatalf("wrong digest = %v", err)
	}
}
