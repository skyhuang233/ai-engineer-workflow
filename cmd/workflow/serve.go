package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skyhuang233/workflow/internal/admission"
	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func serveCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	startupTimeout := flags.Duration("startup-timeout", 30*time.Second, "health readiness timeout")
	listen := flags.String("listen", "127.0.0.1:0", "loopback health endpoint")
	approvedDigest := flags.String("approved-plan-digest", "", "approved Platform Bootstrap Plan digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("workflow serve accepts flags only")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	installation, err := database.PlatformInstallation(context.Background())
	if err != nil {
		database.Close()
		return fmt.Errorf("read Platform Installation: %w", err)
	}
	if err := requireInstalledWorkflowIdentity(installation.PlatformVersion, installation.WorkflowCLISHA256); err != nil {
		database.Close()
		return err
	}
	digest := strings.ToLower(strings.TrimSpace(*approvedDigest))
	if digest == "" {
		digest = installation.ControlPlanePlanDigestSHA256
	}
	if digest == "" || digest != installation.ControlPlanePlanDigestSHA256 {
		database.Close()
		return errors.New("Control Plane launch digest is not the durable Platform Installation authorization")
	}
	if err := database.Close(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	record, err := controlplane.Start(context.Background(), controlplane.StartOptions{Layout: layout, Executable: executable, PlatformVersion: installation.PlatformVersion, ApprovedPlanDigestSHA256: digest, Listen: *listen, Timeout: *startupTimeout})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"status": "ready", "runtime": record})
}

// serveChildCommand is intentionally absent from usage. It is the one
// foreground process mode and never calls the detached launcher.
func serveChildCommand(args []string) error {
	flags := flag.NewFlagSet("serve-child", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", "", "absolute Workflow Home")
	listenAddress := flags.String("listen", "127.0.0.1:0", "loopback health endpoint")
	approvedDigest := flags.String("approved-plan-digest", "", "approved Platform Bootstrap Plan digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *homeOverride == "" || *approvedDigest == "" {
		return errors.New("invalid internal Control Plane child invocation")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	installation, err := database.PlatformInstallation(context.Background())
	if err != nil {
		database.Close()
		return fmt.Errorf("read Platform Installation: %w", err)
	}
	digest := strings.ToLower(strings.TrimSpace(*approvedDigest))
	if err := requireInstalledWorkflowIdentity(installation.PlatformVersion, installation.WorkflowCLISHA256); err != nil {
		database.Close()
		return err
	}
	if digest == "" || digest != installation.ControlPlanePlanDigestSHA256 {
		database.Close()
		return errors.New("Control Plane child launch digest is not the durable Platform Installation authorization")
	}
	if err := database.Close(); err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	runtimeLock, err := startup.AcquireControlPlaneRuntimeLock(layout.Root)
	if err != nil {
		return err
	}
	defer runtimeLock.Close()
	host, _, err := net.SplitHostPort(*listenAddress)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("Control Plane health listener must be an explicit loopback address")
	}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return fmt.Errorf("bind Control Plane health endpoint: %w", err)
	}
	defer listener.Close()
	loops, closeLoops, err := currentControlPlaneLoops(context.Background(), layout)
	if err != nil {
		return err
	}
	defer closeLoops()
	started, live, err := controlplane.OSProcessIdentity(os.Getpid())
	if err != nil || !live {
		return fmt.Errorf("resolve Control Plane process start identity: %w", err)
	}
	baseURL := "http://" + listener.Addr().String()
	record := controlplane.RuntimeRecord{PID: os.Getpid(), PlatformVersion: Version, ProcessStartedAt: started, Endpoints: controlplane.Endpoints{Health: baseURL + "/health", Shutdown: baseURL + "/shutdown"}, ApprovedPlanDigestSHA256: digest}
	if err := controlplane.WriteRuntimeRecord(layout, record); err != nil {
		return err
	}
	database, err = store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	endpoints, _ := json.Marshal(record.Endpoints)
	now := time.Now().UTC()
	if err := database.RecordControlPlaneRuntimeObservation(context.Background(), store.ControlPlaneRuntimeObservation{PID: record.PID, ProcessStartedAt: record.ProcessStartedAt, EndpointsJSON: string(endpoints), PlatformVersion: record.PlatformVersion, PlanDigestSHA256: record.ApprovedPlanDigestSHA256, ObservedAt: now}); err != nil {
		database.Close()
		return err
	}
	if err := database.Close(); err != nil {
		return err
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return (controlplane.Service{Listener: listener, Identity: record.Identity(), Loops: loops}).Run(ctx)
}

func requireInstalledWorkflowVersion(installed string) error {
	if strings.TrimSpace(installed) == "" || Version != strings.TrimSpace(installed) {
		return fmt.Errorf("Workflow CLI published version %q differs from durable Platform Installation version %q", Version, installed)
	}
	return nil
}

var workflowExecutableSHA256 = currentWorkflowExecutableSHA256

func requireInstalledWorkflowIdentity(installedVersion, installedSHA256 string) error {
	if err := requireInstalledWorkflowVersion(installedVersion); err != nil {
		return err
	}
	if len(installedSHA256) != 64 || strings.ToLower(installedSHA256) != installedSHA256 {
		return errors.New("durable Workflow CLI checksum identity is invalid")
	}
	for _, character := range installedSHA256 {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return errors.New("durable Workflow CLI checksum identity is invalid")
		}
	}
	liveSHA256, err := workflowExecutableSHA256()
	if err != nil {
		return errors.New("cannot verify the current Workflow CLI executable checksum")
	}
	if liveSHA256 != installedSHA256 {
		return errors.New("Workflow CLI executable checksum differs from durable Platform Installation")
	}
	return nil
}

func currentWorkflowExecutableSHA256() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func currentControlPlaneLoops(ctx context.Context, layout workflowhome.Layout) ([]controlplane.Loop, func() error, error) {
	database, err := store.Open(ctx, filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return nil, nil, err
	}
	fail := func(value error) ([]controlplane.Loop, func() error, error) {
		_ = database.Close()
		return nil, nil, value
	}
	verification, err := database.GitHubPATVerification(ctx)
	if err != nil || verification.Status != "verified" {
		return fail(errors.Join(errors.New("Control Plane PAT verification is unavailable"), err))
	}
	contractRaw, err := os.ReadFile(filepath.Join(layout.Config, "platform-setup-contract.json"))
	if err != nil {
		return fail(err)
	}
	var contract platformrelease.PlatformSetupContract
	if err := json.Unmarshal(contractRaw, &contract); err != nil {
		return fail(err)
	}
	if err := contract.Validate(); err != nil {
		return fail(err)
	}
	admissions := admission.Service{Store: database, Verifier: admission.DynamicGitHubVerifier{Store: database, Contract: contract}}
	// Complete one verification pass before advertising health. Individual
	// repository drift is durably suspended and does not fail unrelated repos.
	if err := admissions.VerifyAll(ctx); err != nil {
		return fail(err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	admissionLoop := func(loopCtx context.Context) error { return admissions.Run(loopCtx, time.Minute) }
	repositoryLoop := func(loopCtx context.Context) error {
		return (controlplane.RepositorySupervisor{Store: database, Runner: commandRepositoryRunner{Executable: executable, Layout: layout, Owner: verification.Owner}, Interval: time.Second}).Run(loopCtx)
	}
	return []controlplane.Loop{admissionLoop, repositoryLoop}, database.Close, nil
}
