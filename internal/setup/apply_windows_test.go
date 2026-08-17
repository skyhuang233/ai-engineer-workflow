//go:build windows

package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestWindowsFreshInstallAuthorizesControlPlaneBeforeLaunchAndEndsReady(t *testing.T) {
	ctx := context.Background()
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "WorkflowHome"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), workflowhome.ExecutableName)
	cli := []byte("fresh Windows workflow CLI")
	if err := os.WriteFile(source, cli, 0o700); err != nil {
		t.Fatal(err)
	}
	cliSum := sha256.Sum256(cli)
	cliDigest := hex.EncodeToString(cliSum[:])
	if err := (workflowhome.Installation{Layout: layout}).InstallVersion("1.0.0", source, cliDigest); err != nil {
		t.Fatal(err)
	}

	contractRaw := validPlatformSetupContractJSON(t)
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
		PlanID:        "windows-fresh-install-control-plane-authorization",
		Kind:          setupcontract.PlatformBootstrap,
		Target:        setupcontract.Target{WorkflowHome: layout.Root},
		Preconditions: []setupcontract.Precondition{
			{ID: "release", Kind: "platform_release", Subject: "platform-v1.0.0", Expected: repeat("a", 64)},
			{ID: "contract", Kind: "platform_setup_contract", Subject: "platform-v1.0.0", Expected: contractDigest},
		},
		Effects: []setupcontract.Effect{
			{ID: "record", Kind: "platform_installation", Subject: layout.Root, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": repeat("a", 64), "platform_setup_contract_json": string(contractCanonical), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_json": string(bundleCanonical), "release_bundled_files_digest": bundleDigest}},
			{ID: "start", Kind: "control_plane", Subject: layout.Root, Action: "start", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": repeat("a", 64), "platform_setup_contract_digest": contractDigest, "workflow_cli_sha256": cliDigest, "release_bundled_files_digest": bundleDigest}},
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

	adapter := HostAdapter{Layout: layout, PlanDigest: digest}
	adapter.StartControlPlane = func(_ context.Context, options controlplane.StartOptions) (controlplane.RuntimeRecord, error) {
		database, openErr := store.OpenReadOnly(ctx, filepath.Join(layout.State, "workflow.db"))
		if openErr != nil {
			return controlplane.RuntimeRecord{}, openErr
		}
		installation, readErr := database.PlatformInstallation(ctx)
		closeErr := database.Close()
		if readErr != nil || closeErr != nil {
			return controlplane.RuntimeRecord{}, errors.Join(readErr, closeErr)
		}
		if installation.ControlPlanePlanDigestSHA256 == "" || installation.ControlPlanePlanDigestSHA256 != digest || options.ApprovedPlanDigestSHA256 != digest {
			return controlplane.RuntimeRecord{}, errors.New("Control Plane launch started before durable Platform Installation authorization")
		}
		record := controlplane.RuntimeRecord{PID: 4242, PlatformVersion: options.PlatformVersion, ProcessStartedAt: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC), Endpoints: controlplane.Endpoints{Health: "http://127.0.0.1:4242/health", Shutdown: "http://127.0.0.1:4242/shutdown"}, ApprovedPlanDigestSHA256: options.ApprovedPlanDigestSHA256}
		return record, controlplane.WriteRuntimeRecord(layout, record)
	}
	adapter.InspectControlPlane = func(_ context.Context, record *controlplane.RuntimeRecord) controlplane.Observation {
		return controlplane.Observation{State: controlplane.StateReady, Record: record}
	}
	engine := Engine{
		Adapter:                      adapter,
		PlatformPreconditionVerifier: passingPlatformPreconditionVerifier,
		ExpectedResultVerifier: func(ctx context.Context, _ setupcontract.Plan, _ setupcontract.ExpectedResult) error {
			database, openErr := store.OpenReadOnly(ctx, filepath.Join(layout.State, "workflow.db"))
			if openErr != nil {
				return openErr
			}
			defer database.Close()
			installation, readErr := database.PlatformInstallation(ctx)
			if readErr != nil {
				return readErr
			}
			runtimeRecord, runtimeErr := controlplane.ReadRuntimeRecord(layout)
			if runtimeErr != nil {
				return runtimeErr
			}
			if installation.ControlPlanePlanDigestSHA256 != digest || runtimeRecord.ApprovedPlanDigestSHA256 != digest {
				return errors.New("fresh Windows Platform did not reach ready with one durable authorization digest")
			}
			return nil
		},
	}
	result, applyErr := engine.Apply(ctx, canonical, digest)
	if applyErr != nil || result.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("fresh Windows apply result=%#v err=%v", result, applyErr)
	}
}
