package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestRuntimeConfigureCreatesCleanRepositoryRuntimeDirectories(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "workflow-home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	generation := strings.Repeat("a", 64)
	platformPath := filepath.Join(layout.Root, "platform")
	generationPath := filepath.Join(platformPath, "generations", generation)
	if err := os.MkdirAll(generationPath, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(generationPath, "workflow.db")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := t.TempDir()
	if err := database.RecordRepositoryAdmission(ctx, store.RepositoryAdmission{
		Repository: "owner/repository", OnboardingPlanDigestSHA256: strings.Repeat("b", 64),
		ContractVersion: "1", ManifestDigestSHA256: strings.Repeat("c", 64),
		Eligible: true, VerifiedAt: time.Now().UTC(),
	}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.RecordRepositoryRuntimeConfiguration(ctx, store.RepositoryRuntimeConfiguration{
		Repository: "owner/repository", DefaultBranch: "main", SourcePath: sourcePath,
		GitHubAPIURL: "https://api.github.com", PollInterval: time.Minute,
		WorkspaceRetention: 7 * 24 * time.Hour, MaxParallelRuns: 1, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	active := launcher.Active{
		SchemaVersion: launcher.ProtocolVersion, Generation: generation, Version: "0.0.1",
		BundleDigest: "sha256:" + generation, AttemptID: "attempt", ConsentID: "consent", Readiness: "ready",
	}
	activeJSON, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(platformPath, "active.json"), activeJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account","id_token":"id","refresh_token":"refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runtimeConfigureCommand([]string{
		"--workflow-home", layout.Root,
		"--source", sourcePath,
		"--root", "2",
		"--codex-auth-file", authFile,
	}, &output); err != nil {
		t.Fatal(err)
	}

	database, err = store.OpenActivated(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	configuration, err := database.RepositoryRuntimeConfiguration(ctx, "owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"workspace root": configuration.WorkspaceRoot,
		"state root":     configuration.StateRoot,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s %q was not created: %v", name, path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s %q is not a directory", name, path)
		}
	}
}
