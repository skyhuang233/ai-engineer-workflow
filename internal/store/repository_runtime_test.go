package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoryRuntimeConfigurationRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if err := db.RecordRepositoryAdmission(ctx, RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: repeatHex('a'), ContractVersion: "1", ManifestDigestSHA256: repeatHex('b'), Eligible: true, VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	value := RepositoryRuntimeConfiguration{
		Repository: "owner/repo", DefaultBranch: "main", SourcePath: filepath.Join(t.TempDir(), "repo"),
		RootIssueNumber: 42, WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"), StateRoot: filepath.Join(t.TempDir(), "codex"),
		CodexAuthFile: filepath.Join(t.TempDir(), "auth.json"), GitHubAPIURL: "https://api.github.com",
		PollInterval: time.Minute, WorkspaceRetention: 7 * 24 * time.Hour, MaxParallelRuns: 2, UpdatedAt: now,
	}
	if err := db.RecordRepositoryRuntimeConfiguration(ctx, value); err != nil {
		t.Fatal(err)
	}
	got, err := db.RepositoryRuntimeConfiguration(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository != value.Repository || got.DefaultBranch != value.DefaultBranch || got.RootIssueNumber != 42 || got.PollInterval != time.Minute || got.WorkspaceRetention != 7*24*time.Hour || got.MaxParallelRuns != 2 || !got.UpdatedAt.Equal(now) {
		t.Fatalf("configuration = %#v", got)
	}
	values, err := db.RepositoryRuntimeConfigurations(ctx)
	if err != nil || len(values) != 1 {
		t.Fatalf("configurations = %#v, %v", values, err)
	}
}

func TestRepositoryRuntimeConfigurationMarksIncompleteValuesNotReady(t *testing.T) {
	value := RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: time.Hour, MaxParallelRuns: 1, UpdatedAt: time.Now().UTC()}
	if !errors.Is(value.Ready(), ErrRepositoryRuntimeNotConfigured) {
		t.Fatalf("ready = %v", value.Ready())
	}
}

func TestRepositoryRuntimeConfigurationPreservesIncompleteHostPaths(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if err := db.RecordRepositoryAdmission(ctx, RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: repeatHex('a'), ContractVersion: "1", ManifestDigestSHA256: repeatHex('b'), Eligible: true, VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	value := RepositoryRuntimeConfiguration{
		Repository: "owner/repo", DefaultBranch: "main", GitHubAPIURL: "https://api.github.com",
		PollInterval: time.Minute, WorkspaceRetention: time.Hour, MaxParallelRuns: 1, UpdatedAt: now,
	}
	if err := db.RecordRepositoryRuntimeConfiguration(ctx, value); err != nil {
		t.Fatal(err)
	}
	got, err := db.RepositoryRuntimeConfiguration(ctx, value.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourcePath != "" || got.WorkspaceRoot != "" || got.StateRoot != "" || got.CodexAuthFile != "" {
		t.Fatalf("incomplete paths were rewritten: %#v", got)
	}
	if !errors.Is(got.Ready(), ErrRepositoryRuntimeNotConfigured) {
		t.Fatalf("incomplete configuration became ready: %v", got.Ready())
	}
}

func TestRepositoryAdmissionWithInitialRuntimeConfigurationRollsBackAndRetries(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	admission := RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: repeatHex('a'), ContractVersion: "1", ManifestDigestSHA256: repeatHex('b'), Eligible: true, VerifiedAt: now}
	runtime := RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "main", SourcePath: filepath.Join(t.TempDir(), "repo"), GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute, WorkspaceRetention: 7 * 24 * time.Hour, MaxParallelRuns: 1, UpdatedAt: now}
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER fail_runtime_seed BEFORE INSERT ON repository_runtime_configurations BEGIN SELECT RAISE(ABORT, 'runtime seed failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx, admission, runtime); err == nil {
		t.Fatal("runtime seed failure committed Repository Admission")
	}
	if _, err := db.RepositoryAdmission(ctx, admission.Repository); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transaction left Repository Admission: %v", err)
	}
	if _, err := db.RepositoryRuntimeConfiguration(ctx, runtime.Repository); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transaction left runtime configuration: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `DROP TRIGGER fail_runtime_seed`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx, admission, runtime); err != nil {
		t.Fatalf("retry admission: %v", err)
	}
	if _, err := db.RepositoryAdmission(ctx, admission.Repository); err != nil {
		t.Fatalf("retry did not record Repository Admission: %v", err)
	}
	if _, err := db.RepositoryRuntimeConfiguration(ctx, runtime.Repository); err != nil {
		t.Fatalf("retry did not seed runtime configuration: %v", err)
	}
}

func TestRepositoryAdmissionWithInitialRuntimeConfigurationPreservesExistingRuntime(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	admission := RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: repeatHex('a'), ContractVersion: "1", ManifestDigestSHA256: repeatHex('b'), Eligible: true, VerifiedAt: now}
	existing := RepositoryRuntimeConfiguration{Repository: "owner/repo", DefaultBranch: "trunk", SourcePath: filepath.Join(t.TempDir(), "existing-repo"), RootIssueNumber: 42, WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"), StateRoot: filepath.Join(t.TempDir(), "state"), CodexAuthFile: filepath.Join(t.TempDir(), "auth.json"), GitHubAPIURL: "https://github.example/api/v3", PollInterval: 2 * time.Minute, WorkspaceRetention: 24 * time.Hour, MaxParallelRuns: 3, UpdatedAt: now}
	if err := db.RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx, admission, existing); err != nil {
		t.Fatal(err)
	}
	updatedAdmission := admission
	updatedAdmission.ContractVersion = "2"
	updatedAdmission.ManifestDigestSHA256 = repeatHex('c')
	updatedAdmission.VerifiedAt = now.Add(time.Hour)
	replacementSeed := existing
	replacementSeed.DefaultBranch = "main"
	replacementSeed.SourcePath = filepath.Join(t.TempDir(), "replacement-repo")
	replacementSeed.RootIssueNumber = 0
	replacementSeed.UpdatedAt = now.Add(time.Hour)
	if err := db.RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx, updatedAdmission, replacementSeed); err != nil {
		t.Fatal(err)
	}
	gotAdmission, err := db.RepositoryAdmission(ctx, admission.Repository)
	if err != nil || gotAdmission.ContractVersion != "2" || gotAdmission.ManifestDigestSHA256 != repeatHex('c') {
		t.Fatalf("updated Repository Admission = %#v, %v", gotAdmission, err)
	}
	gotRuntime, err := db.RepositoryRuntimeConfiguration(ctx, existing.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if gotRuntime != existing {
		t.Fatalf("existing runtime configuration changed: got %#v want %#v", gotRuntime, existing)
	}
}

func TestMigration58BackfillsIdentitySourceBranchAndExistingRoot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.RecordRepositoryAdmission(ctx, RepositoryAdmission{Repository: "owner/legacy", OnboardingPlanDigestSHA256: repeatHex('c'), ContractVersion: "1", ManifestDigestSHA256: repeatHex('d'), Eligible: true, VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	canonical := `{"schema_version":1,"plan_id":"legacy","kind":"repository_onboarding","target":{"workflow_home":"C:\\Workflow","repository_path":"C:\\repo","github_repository":"owner/legacy"},"preconditions":[],"effects":[{"id":"admit","kind":"repository_admission","subject":"owner/legacy","action":"verify_and_record","parameters":{"default_branch":"trunk","manifest_digest":"` + repeatHex('d') + `","contract_version":"1"}}],"expected_results":[]}`
	if err := db.RecordSetupPlan(ctx, SetupPlanRecord{PlanID: "legacy", Kind: "repository_onboarding", SchemaVersion: 1, Target: `C:\repo`, DigestSHA256: repeatHex('e'), CanonicalJSON: canonical, Projection: "legacy", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO plans(repository,root_issue_id,root_issue_number,state,created_at,updated_at) VALUES('owner/legacy',77,23,'active',?,?)`, formatTimestamp(now), formatTimestamp(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `DROP TABLE repository_runtime_configurations`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version>=58`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := db.RepositoryRuntimeConfiguration(ctx, "owner/legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultBranch != "trunk" || got.SourcePath != `C:\repo` || got.RootIssueNumber != 23 {
		t.Fatalf("backfill = %#v", got)
	}
	if !errors.Is(got.Ready(), ErrRepositoryRuntimeNotConfigured) {
		t.Fatalf("legacy configuration should await host-path completion: %v", got.Ready())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
