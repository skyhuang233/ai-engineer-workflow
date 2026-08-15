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
	if _, err := db.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=58`); err != nil {
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
