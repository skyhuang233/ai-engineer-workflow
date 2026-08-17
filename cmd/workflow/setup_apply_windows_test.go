//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	setupengine "github.com/skyhuang233/workflow/internal/setup"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestWindowsFreshSetupApplyStartsRealControlPlaneWithDurableAuthorization(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh Workflow Home exists before installation: %v", err)
	}

	_, current, _, _ := runtime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	source := filepath.Join(t.TempDir(), workflowhome.ExecutableName)
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-buildid= -X main.Version=1.0.0", "-o", source, "./cmd/workflow")
	build.Dir = repositoryRoot
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build real workflow.exe: %v\n%s", buildErr, output)
	}
	cli, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cliSum := sha256.Sum256(cli)
	cliDigest := hex.EncodeToString(cliSum[:])
	if err := (workflowhome.Installation{Layout: layout}).InstallVersion("1.0.0", source, cliDigest); err != nil {
		t.Fatal(err)
	}

	database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	verification := store.GitHubPATVerification{FingerprintSHA256: strings.Repeat("f", 64), Login: "owner", UserID: 1, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: layout.CredentialFile, Status: "verified", VerifiedAt: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)}
	if err := database.RecordGitHubPATVerification(ctx, verification); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	contract := platformrelease.PlatformSetupContract{
		WorkflowHomeDefault: `%LOCALAPPDATA%\AgentWorkflow`,
		Credential:          platformrelease.CredentialContract{Kind: "classic-pat", RequiredScopes: []string{"repo", "workflow"}, OwnerBinding: "single-owner", PlaintextRelativePath: `state\credentials\github.pat`},
		Docker:              platformrelease.DockerDependency{Version: "4.86.0", InstallerURL: "https://example.test/docker.exe", WindowsAMD64SHA256: strings.Repeat("d", 64)},
		Worker:              platformrelease.WorkerPin{Image: "ghcr.io/owner/worker@sha256:" + strings.Repeat("e", 64)},
		SkillBundle:         platformrelease.SkillBundleContract{Version: "1.0.0", InstallScope: "user", ManagedSkills: []string{"implement"}},
		RepositoryContract:  platformrelease.RepositoryContractPin{Version: "1", ManifestPath: ".workflow/repository.json", CheckName: "workflow-contract", Labels: []platformrelease.RepositoryLabel{{Name: "workflow:plan", Color: "0e8a16", Description: "plan"}}},
	}
	contractRaw, err := json.Marshal(contract)
	if err != nil || contract.Validate() != nil {
		t.Fatalf("test Platform Setup Contract: %v", err)
	}
	contractCanonical, contractDigest, err := setupcontract.Canonicalize(contractRaw)
	if err != nil {
		t.Fatal(err)
	}
	bundleRaw, err := json.Marshal([]platformrelease.BundledFile{{Path: "bin/workflow.exe", SHA256: cliDigest}})
	if err != nil {
		t.Fatal(err)
	}
	bundleCanonical, bundleDigest, err := setupcontract.Canonicalize(bundleRaw)
	if err != nil {
		t.Fatal(err)
	}
	plan := setupcontract.Plan{
		SchemaVersion: 1,
		PlanID:        "windows-real-child-fresh-install",
		Kind:          setupcontract.PlatformBootstrap,
		Target:        setupcontract.Target{WorkflowHome: layout.Root},
		Preconditions: []setupcontract.Precondition{
			{ID: "release", Kind: "platform_release", Subject: "platform-v1.0.0", Expected: strings.Repeat("a", 64)},
			{ID: "contract", Kind: "platform_setup_contract", Subject: "platform-v1.0.0", Expected: contractDigest},
		},
		Effects: []setupcontract.Effect{
			{ID: "record", Kind: "platform_installation", Subject: layout.Root, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": strings.Repeat("a", 64), "platform_setup_contract_json": string(contractCanonical), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_json": string(bundleCanonical), "release_bundled_files_digest": bundleDigest}},
			{ID: "start", Kind: "control_plane", Subject: layout.Root, Action: "start", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": strings.Repeat("a", 64), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_digest": bundleDigest}},
		},
		ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: layout.Root, Expected: "ready"}},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	_, canonical, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(t.TempDir(), "platform-bootstrap-plan.json")
	if err := os.WriteFile(planPath, canonical, 0o600); err != nil {
		t.Fatal(err)
	}

	originalPreconditions, originalReady := verifyPlatformPreconditionsForSetup, verifyPlatformReadyForApply
	t.Cleanup(func() {
		verifyPlatformPreconditionsForSetup, verifyPlatformReadyForApply = originalPreconditions, originalReady
	})
	verifyPlatformPreconditionsForSetup = func(context.Context, *store.Store, workflowhome.Layout, setupengine.HostAdapter, setupcontract.Plan) error {
		return nil
	}
	readyObserved := false
	verifyPlatformReadyForApply = func(ctx context.Context, database *store.Store, gotLayout workflowhome.Layout, _ *platformCleanupTracker) error {
		installation, readErr := database.PlatformInstallation(ctx)
		if readErr != nil {
			return readErr
		}
		record, runtimeErr := controlplane.ReadRuntimeRecord(gotLayout)
		if runtimeErr != nil {
			return runtimeErr
		}
		observation := (controlplane.Inspector{}).Inspect(ctx, &record)
		if installation.ControlPlanePlanDigestSHA256 == "" || installation.ControlPlanePlanDigestSHA256 != digest || record.ApprovedPlanDigestSHA256 != digest || observation.State != controlplane.StateReady {
			return errors.New("real Control Plane is not ready with the durable approved plan digest")
		}
		if bindingErr := verifyRuntimePlanBinding(ctx, database, record, installation); bindingErr != nil {
			return bindingErr
		}
		readyObserved = true
		return nil
	}
	t.Cleanup(func() {
		record, readErr := controlplane.ReadRuntimeRecord(layout)
		if readErr != nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if stopErr := controlplane.Stop(stopCtx, record, controlplane.Inspector{}); stopErr != nil {
			t.Errorf("stop real Control Plane: %v", stopErr)
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			_, live, identityErr := controlplane.OSProcessIdentity(record.PID)
			if identityErr == nil && !live {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("real Control Plane process %d did not exit", record.PID)
	})

	var applyOutput bytes.Buffer
	if err := runSetupApply([]string{"--plan", planPath, "--approved-digest", digest}, strings.NewReader(""), &applyOutput); err != nil {
		t.Fatalf("real setup apply: %v\n%s", err, applyOutput.String())
	}
	var response setupResponse
	if err := json.Unmarshal(applyOutput.Bytes(), &response); err != nil || response.Status != string(setupcontract.ExecutionSucceeded) || !readyObserved {
		t.Fatalf("setup apply response=%s ready=%t decode=%v", applyOutput.String(), readyObserved, err)
	}
	var statusOutput bytes.Buffer
	if err := runtimeStatusCommand([]string{"--workflow-home", layout.Root}, &statusOutput); err != nil {
		t.Fatal(err)
	}
	var status controlplane.Observation
	if err := json.Unmarshal(statusOutput.Bytes(), &status); err != nil || status.State != controlplane.StateReady || status.Record == nil || status.Record.ApprovedPlanDigestSHA256 != digest {
		t.Fatalf("workflow status=%s decode=%v", statusOutput.String(), err)
	}
}
