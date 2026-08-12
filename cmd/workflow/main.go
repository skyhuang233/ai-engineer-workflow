package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skyhuang233/workflow/internal/agent"
	candidateoutput "github.com/skyhuang233/workflow/internal/candidate"
	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubapp"
	"github.com/skyhuang233/workflow/internal/githubcontract"
	deliveryisolation "github.com/skyhuang233/workflow/internal/isolation"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/scheduler"
	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

const (
	defaultControlPlaneDatabase = "workflow.db"
	doctorVerificationTimeout   = 10 * time.Minute
	restoreIsolationTimeout     = 30 * time.Second
)

func defaultCodexAuthFile() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, codexauth.FileName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", codexauth.FileName)
}

func controlPlaneContainerID(databasePath string) string {
	canonical, err := startup.DatabaseIdentity(databasePath)
	if err != nil || canonical == "" {
		canonical = filepath.Clean(databasePath)
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

type admittedCredential string

func (c admittedCredential) Get(context.Context, string) (string, error) {
	return string(c), nil
}

func (admittedCredential) Set(context.Context, string, string) error {
	return errors.New("admitted credentials cannot be replaced")
}

type githubTokenProvider interface {
	Token(context.Context) (string, error)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "doctor":
		runDoctor(os.Args[2:])
	case "credential":
		credentialCommand()
	case "run-ticket":
		runTicket(os.Args[2:])
	case "gateway":
		runGateway(os.Args[2:])
	case "poll-github":
		runPollGitHub(os.Args[2:])
	case "reconcile-delivered":
		runReconcileDelivered(os.Args[2:])
	case "answer-inbox":
		runAnswerInbox(os.Args[2:])
	case "recover-inbox-delivery":
		runRecoverInboxDelivery(os.Args[2:])
	case "backup":
		runBackup(os.Args[2:])
	case "restore":
		runRestore(os.Args[2:])
	case "drill-backup":
		runBackupDrill(os.Args[2:])
	case "metrics":
		runMetrics(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  workflow doctor --workflow-repository owner/repository [--config path] [--database path] [--codex-auth-file path] [--report path]")
	fmt.Fprintln(os.Stderr, "  workflow credential provision --app-id <GITHUB_APP_ID> [--config path] [--database path]")
	fmt.Fprintln(os.Stderr, "  workflow run-ticket [options]")
	fmt.Fprintln(os.Stderr, "  workflow gateway [options]")
	fmt.Fprintln(os.Stderr, "  workflow poll-github [options]")
	fmt.Fprintln(os.Stderr, "  workflow reconcile-delivered [options]")
	fmt.Fprintln(os.Stderr, "  workflow answer-inbox [options]")
	fmt.Fprintln(os.Stderr, "  workflow recover-inbox-delivery [options]")
	fmt.Fprintln(os.Stderr, "  workflow backup [--database path] [--output path]")
	fmt.Fprintln(os.Stderr, "  workflow restore --backup path [--database path]")
	fmt.Fprintln(os.Stderr, "  workflow drill-backup --backup path")
	fmt.Fprintln(os.Stderr, "  workflow metrics [--database path] [--backup path]")
}

func runBackup(args []string) {
	flags := flag.NewFlagSet("backup", flag.ExitOnError)
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	outputPath := flags.String("output", "", "online SQLite backup destination")
	_ = flags.Parse(args)
	if *outputPath == "" {
		*outputPath = *databasePath + ".backup"
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	metadata, err := db.CreateOnlineBackup(context.Background(), *outputPath, time.Now().UTC())
	if err != nil {
		fail(err)
	}
	writeJSON(os.Stdout, metadata)
	writeStructuredLog("sqlite_backup", metadata)
}

func runRestore(args []string) {
	flags := flag.NewFlagSet("restore", flag.ExitOnError)
	backupPath := flags.String("backup", "", "verified SQLite online backup")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "restored SQLite control-plane database")
	_ = flags.Parse(args)
	if *backupPath == "" {
		fmt.Fprintln(os.Stderr, "restore requires backup")
		os.Exit(2)
	}
	lock, err := startup.AcquireLock(*databasePath)
	if err != nil {
		fail(err)
	}
	defer lock.Close()
	ctx := context.Background()
	isolator := worker.DockerRuntime{ControlPlaneID: controlPlaneContainerID(*databasePath)}
	if err := restoreControlPlane(ctx, *backupPath, *databasePath, isolator, time.Now().UTC()); err != nil {
		fail(err)
	}
	writeStructuredLog("sqlite_restore_reconciled", map[string]string{"backup": *backupPath, "database": *databasePath})
}

type restoreContainerIsolator interface {
	worker.ContainerIsolator
	IsolateControlPlaneDeliveryContainers(context.Context) error
}

func restoreControlPlane(ctx context.Context, backupPath, databasePath string, isolator restoreContainerIsolator, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(databasePath), filepath.Base(databasePath)+".restore-staged-*.db")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	if err := staged.Close(); err != nil {
		return err
	}
	defer os.Remove(stagedPath)
	defer os.Remove(stagedPath + "-wal")
	defer os.Remove(stagedPath + "-shm")
	if err := store.RestoreBackup(ctx, backupPath, stagedPath); err != nil {
		return err
	}
	db, err := store.Open(ctx, stagedPath)
	if err != nil {
		return err
	}
	if err := db.ReconcileRestoredControlPlaneDryRun(ctx, now); err != nil {
		db.Close()
		return err
	}
	restoreBarrier, err := startup.AcquireRestoreBarrier(ctx, databasePath)
	if err != nil {
		db.Close()
		return err
	}
	defer restoreBarrier.Close()
	if err := isolateCurrentControlPlane(ctx, databasePath, isolator); err != nil {
		db.Close()
		return err
	}
	if err := reconcileRestoredControlPlane(ctx, db, isolator, now); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	return store.PublishRestoredBackup(ctx, stagedPath, databasePath)
}

func isolateCurrentControlPlane(ctx context.Context, databasePath string, isolator restoreContainerIsolator) error {
	if _, err := os.Stat(databasePath); errors.Is(err, os.ErrNotExist) {
		return isolateControlPlaneDeliveryContainers(ctx, isolator)
	} else if err != nil {
		return err
	}
	db, err := store.OpenForRestore(ctx, databasePath)
	if err != nil {
		if store.IsDatabaseError(err) || errors.Is(err, store.ErrDatabaseIntegrity) {
			return isolateControlPlaneDeliveryContainers(ctx, isolator)
		}
		return fmt.Errorf("open current Control Plane before restore: %w", err)
	}
	targets, targetErr := db.DeliveryIsolationTargets(ctx)
	if targetErr == nil && len(targets) > 0 {
		_, targetErr = isolateDeliveryTargets(ctx, db, isolator, targets)
	}
	if closeErr := db.Close(); targetErr != nil || closeErr != nil {
		return errors.Join(targetErr, closeErr)
	}
	return isolateControlPlaneDeliveryContainers(ctx, isolator)
}

func isolateDeliveryTargets(ctx context.Context, db *store.Store, isolator worker.ContainerIsolator, targets []store.TicketClaim) ([]store.DeliveryIsolationProof, error) {
	if isolator == nil {
		return nil, errors.New("restore cannot isolate an active Delivery Controller")
	}
	return deliveryisolation.DeliveryControllers(ctx, db, isolator, targets)
}

func isolateControlPlaneDeliveryContainers(ctx context.Context, isolator restoreContainerIsolator) error {
	if isolator == nil {
		return errors.New("restore cannot isolate active Delivery Controllers without the current database")
	}
	isolationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreIsolationTimeout)
	defer cancel()
	if err := isolator.IsolateControlPlaneDeliveryContainers(isolationCtx); err != nil {
		return fmt.Errorf("isolate Control Plane Delivery Controllers: %w", err)
	}
	return nil
}

func reconcileRestoredControlPlane(ctx context.Context, db *store.Store, isolator restoreContainerIsolator, now time.Time) error {
	var isolated []store.DeliveryIsolationProof
	for {
		err := db.ReconcileRestoredControlPlane(ctx, now, isolated...)
		var isolation *store.DeliveryIsolationRequired
		if !errors.As(err, &isolation) {
			return err
		}
		if isolator == nil {
			return errors.Join(err, errors.New("restore cannot isolate an active Delivery Controller"))
		}
		fenced, isolateErr := isolateDeliveryTargets(ctx, db, isolator, isolation.Targets)
		if isolateErr != nil {
			return errors.Join(err, isolateErr)
		}
		isolated = append(isolated, fenced...)
	}
}

func runBackupDrill(args []string) {
	flags := flag.NewFlagSet("drill-backup", flag.ExitOnError)
	backupPath := flags.String("backup", "", "verified SQLite online backup")
	_ = flags.Parse(args)
	if *backupPath == "" {
		fmt.Fprintln(os.Stderr, "drill-backup requires backup")
		os.Exit(2)
	}
	drill, err := store.DrillBackup(context.Background(), *backupPath, time.Now().UTC())
	if err != nil {
		fail(err)
	}
	writeJSON(os.Stdout, drill)
	writeStructuredLog("sqlite_backup_drill", drill)
}

func runMetrics(args []string) {
	flags := flag.NewFlagSet("metrics", flag.ExitOnError)
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	backupPath := flags.String("backup", "", "verified SQLite online backup")
	_ = flags.Parse(args)
	if *backupPath == "" {
		*backupPath = *databasePath + ".backup"
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	metrics, err := db.OperationalMetrics(context.Background(), *backupPath, time.Now().UTC())
	if err != nil {
		fail(err)
	}
	writeJSON(os.Stdout, metrics)
	writeStructuredLog("sqlite_operational_metrics", metrics)
}

func writeJSON(destination *os.File, value any) {
	if err := json.NewEncoder(destination).Encode(value); err != nil {
		fail(err)
	}
}

func writeStructuredLog(event string, value any) {
	writeJSON(os.Stderr, map[string]any{"event": event, "data": value})
}

func runDoctor(args []string) {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	reportPath := flags.String("report", "", "optional Markdown report path")
	workflowRepository := flags.String("workflow-repository", "", "GitHub repository containing the Worker publisher workflow")
	codexAuthFile := flags.String("codex-auth-file", defaultCodexAuthFile(), "absolute host ChatGPT auth.json used to seed Worker sessions")
	_ = flags.Parse(args)
	if *workflowRepository == "" {
		fmt.Fprintln(os.Stderr, "doctor requires workflow-repository")
		os.Exit(2)
	}
	if err := codexauth.ValidateChatGPT(*codexAuthFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	database, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()
	provider, verification, privateKeyPEM, err := loadVerifiedGitHubAppProvider(context.Background(), database, config, "", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, persistGitHubAppAdmissionError(context.Background(), database, err, time.Now().UTC()))
		os.Exit(1)
	}
	secret, err := admitControlPlaneGitHubApp(context.Background(), database, provider, func(token string) error {
		client := github.NewClient("", token, nil).WithRepositoryOwner(config.GitHub.Credential.Owner)
		return requireOwnerGuardedControlPlaneRepository(context.Background(), client, config.GitHub.TestRepository)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	releaseFetcher := doctor.ReleaseFetcher{WorkflowRepository: *workflowRepository}
	manifest, manifestJSON, err := releaseFetcher.Fetch(context.Background(), config, secret)
	if err != nil {
		fmt.Fprintln(os.Stderr, persistGitHubAppAdmissionError(context.Background(), database, err, time.Now().UTC()))
		os.Exit(1)
	}
	admittedCredentials := admittedCredential(secret)
	runner := doctor.Runner{Checks: []doctor.Check{
		doctor.CommandCheck{
			CheckName: "Codex CLI",
			Executor:  doctor.OSExecutor{},
			Version: doctor.CommandExpectation{
				Command:      []string{"codex", "--version"},
				Tool:         "codex",
				ExactVersion: config.Codex.Version,
			},
			Capabilities: []doctor.CommandExpectation{{
				Command:  []string{"codex", "exec", "--help"},
				Contains: []string{"resume", "--json", "--output-schema", "--ephemeral"},
			}},
		},
		doctor.WorkerCodexSessionCheck{Executor: doctor.OSExecutor{}, Image: manifest.Image, AuthFile: *codexAuthFile},
		doctor.SQLiteCheck{Path: *databasePath},
		doctor.DockerCheck{Manifest: manifest},
		doctor.WorkerRegistryCheck{Image: manifest.Image},
		doctor.GitHubCredentialCheck{Pin: config.GitHub.Credential, IntegrationRepository: config.GitHub.TestRepository, PrivateKeyPEM: privateKeyPEM, Verification: verification},
		doctor.GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: admittedCredentials},
	}, Secrets: []string{secret}}
	ctx, cancel := context.WithTimeout(context.Background(), doctorVerificationTimeout)
	defer cancel()
	report := runner.Run(ctx)
	if *reportPath != "" {
		if err := os.WriteFile(*reportPath, []byte(report.Markdown()), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(encoded))
	if err := report.AuthenticationFailure(); err != nil {
		fmt.Fprintln(os.Stderr, persistGitHubAppAdmissionError(context.Background(), database, err, time.Now().UTC()))
		os.Exit(1)
	}
	if !report.Passed() {
		os.Exit(1)
	}
	expectedActiveImage := ""
	if active, activeErr := database.ActiveWorkerRelease(context.Background()); activeErr == nil {
		expectedActiveImage = active.ImageReference
	} else if !errors.Is(activeErr, store.ErrNotFound) {
		fmt.Fprintln(os.Stderr, activeErr)
		os.Exit(1)
	}
	currentManifest, currentManifestJSON, err := releaseFetcher.Fetch(context.Background(), config, secret)
	if err != nil {
		fmt.Fprintln(os.Stderr, persistGitHubAppAdmissionError(context.Background(), database, err, time.Now().UTC()))
		os.Exit(1)
	}
	if currentManifest != manifest || string(currentManifestJSON) != string(manifestJSON) {
		fmt.Fprintln(os.Stderr, "Worker Release changed during doctor verification")
		os.Exit(1)
	}
	if err := database.ActivateWorkerReleaseFenced(context.Background(), store.WorkerRelease{
		Version: manifest.WorkerVersion, SourceCommit: manifest.SourceCommit,
		ImageReference: manifest.Image, ManifestJSON: string(manifestJSON),
		VerifiedAt: report.GeneratedAt, ActivatedAt: report.GeneratedAt,
	}, expectedActiveImage); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("activated Worker image %s for new Worker Runs\n", manifest.Image)
}

func credentialCommand() {
	if len(os.Args) < 3 || os.Args[2] != "provision" {
		usage()
		os.Exit(2)
	}
	flags := flag.NewFlagSet("credential provision", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "control-plane SQLite database")
	appID := flags.Int64("app-id", 0, "GitHub App ID")
	_ = flags.Parse(os.Args[3:])
	if *appID <= 0 {
		exitError(errors.New("credential provision requires a positive --app-id"))
	}
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		exitError(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := store.Open(ctx, *databasePath)
	if err != nil {
		exitError(err)
	}
	defer database.Close()
	if err := provisionGitHubApp(ctx, database, config, *appID, nil, githubAppProvisionDependencies{}); err != nil {
		exitError(err)
	}
	fmt.Println("Control Plane GitHub App installation and live contract verified")
}

type githubAppProvisionDependencies struct {
	APIBase string
	Client  *http.Client
	Now     func() time.Time
	Verify  func(context.Context, string, string, string) error
}

func provisionGitHubApp(ctx context.Context, database *store.Store, config doctor.Config, appID int64, privateKeyPEM []byte, dependencies githubAppProvisionDependencies) (resultErr error) {
	owner, err := credentialRotationOwner()
	if err != nil {
		return err
	}
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	rotation, err := database.BeginGatewayCredentialRotation(ctx, owner, "Control Plane GitHub App verification is in progress", now().UTC())
	if err != nil {
		return err
	}
	resumed := false
	defer func() {
		if !resumed {
			resultErr = errors.Join(resultErr, database.EndGatewayCredentialRotation(context.Background(), rotation, now().UTC()))
		}
	}()
	if err := database.RecoverExpiredGatewayDeliveryClaims(ctx, now().UTC()); err != nil {
		return fmt.Errorf("recover expired Gateway delivery claims before credential rotation: %w", err)
	}
	if err := database.WaitForGatewayWritesQuiesced(ctx); err != nil {
		return fmt.Errorf("wait for Gateway writes to finish before credential rotation: %w", err)
	}
	if err := database.RenewGatewayCredentialRotation(ctx, rotation, now().UTC()); err != nil {
		return err
	}
	if len(privateKeyPEM) == 0 {
		privateKeyPEM, err = os.ReadFile(config.GitHub.Credential.PrivateKeyFile)
		if err != nil {
			return fmt.Errorf("read GitHub App private key; Gateway writes remain paused: %w", err)
		}
	}
	installation, err := githubapp.DiscoverInstallation(ctx, githubapp.DiscoveryConfig{
		AppID: appID, PrivateKeyPEM: privateKeyPEM, Owner: config.GitHub.Credential.Owner,
		Repository: config.GitHub.TestRepository, APIBase: dependencies.APIBase, Client: dependencies.Client, Now: now,
	})
	if err != nil {
		return fmt.Errorf("discover GitHub App installation: %w", err)
	}
	provider, err := githubapp.NewProvider(githubapp.Config{
		AppID: appID, InstallationID: installation.ID, PrivateKeyPEM: privateKeyPEM,
		RequiredPermissions: config.GitHub.Credential.Permissions, APIBase: dependencies.APIBase, Client: dependencies.Client, Now: now,
	})
	if err != nil {
		return err
	}
	token, err := provider.Token(ctx)
	if err != nil {
		return err
	}
	verify := dependencies.Verify
	if verify == nil {
		verifier := githubcontract.Verifier{APIBase: dependencies.APIBase, Client: dependencies.Client}
		verify = verifier.Verify
	}
	if err := verify(ctx, token, config.GitHub.Credential.Owner, config.GitHub.TestRepository); err != nil {
		return fmt.Errorf("live contract failed; the existing Control Plane GitHub App verification was not replaced: %w", err)
	}
	if err := database.RenewGatewayCredentialRotation(ctx, rotation, now().UTC()); err != nil {
		return err
	}
	if err := database.RecordGitHubAppVerification(ctx, store.GitHubAppVerification{
		FingerprintSHA256:     privateKeyFingerprint(privateKeyPEM),
		AppID:                 appID,
		InstallationID:        installation.ID,
		Owner:                 config.GitHub.Credential.Owner,
		IntegrationRepository: config.GitHub.TestRepository,
		VerifiedAt:            now().UTC(),
	}); err != nil {
		return fmt.Errorf("record GitHub App verification; Gateway writes remain paused: %w", err)
	}
	if err := database.ResumeGatewayWrites(ctx, rotation, now().UTC()); err != nil {
		return err
	}
	resumed = true
	return nil
}

func privateKeyFingerprint(privateKeyPEM []byte) string {
	digest := sha256.Sum256(privateKeyPEM)
	return hex.EncodeToString(digest[:])
}

func credentialRotationOwner() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "credential-rotation-" + hex.EncodeToString(bytes), nil
}

func exitError(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func runTicket(args []string) {
	flags := flag.NewFlagSet("run-ticket", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	rootNumber := flags.Int64("root", 0, "plan root issue number")
	ticketID := flags.Int64("ticket-id", 0, "GitHub ticket node ID")
	source := flags.String("source", "", "absolute local repository path")
	workspaceRoot := flags.String("workspace-root", "", "absolute Ticket Workspace root")
	stateRoot := flags.String("state-root", "", "absolute Codex state root")
	codexAuthFile := flags.String("codex-auth-file", defaultCodexAuthFile(), "absolute host ChatGPT auth.json used to seed the Ticket Session")
	prompt := flags.String("prompt", "", "Worker prompt")
	reviewFeedback := flags.String("review-feedback", "", "human pull-request feedback to queue for the next revision round")
	branch := flags.String("branch", "", "ticket branch")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	expectedHead := flags.String("expected-remote-head", "", "current remote ticket branch head")
	expectAbsent := flags.Bool("expect-remote-absent", true, "require the ticket branch to be absent")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *ticketID == 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *codexAuthFile == "" || *gatewayURL == "" || (*expectedHead != "") == *expectAbsent {
		fmt.Fprintln(os.Stderr, "run-ticket requires repository, root, ticket-id, source, workspace-root, state-root, ChatGPT authentication, Gateway URL, and exactly one remote-head expectation")
		os.Exit(2)
	}
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	client, provider, err := admittedControlPlaneGitHubClientAndProvider(ctx, db, config, *githubURL, *repository)
	if err != nil {
		fail(err)
	}
	workspaceManager := agent.WorkspaceManager{
		RootDir: *workspaceRoot, CodexStateRoot: *stateRoot, CodexAuthFile: *codexAuthFile,
		RefreshDeliverySource: deliverySourceRefresher(db, provider, *githubURL, *repository),
	}
	snapshot, err := client.ReadPlan(ctx, *repository, *rootNumber)
	if err != nil {
		fail(err)
	}
	version, err := db.CurrentVersion(ctx, *repository, snapshot.Root.ID)
	if err != nil {
		fail(err)
	}
	runtime := worker.DockerRuntime{ControlPlaneID: controlPlaneContainerID(*databasePath)}
	if err := syncReviewFeedback(ctx, db, client, runtime, *repository, version.ID, *ticketID, *reviewFeedback); err != nil {
		fail(err)
	}
	claim, revisionPrompt, err := acquireTicketClaim(ctx, db, version.ID, *ticketID, config.Runtime.MaxWorkerAttempts, time.Now().UTC(), workspaceManager.ProvisionCodexSession)
	if err != nil {
		fail(err)
	}
	*prompt, err = resolveWorkerPrompt(ctx, db, claim, revisionPrompt)
	if err != nil {
		fail(err)
	}
	if *branch == "" {
		*branch = "workflow/ticket-" + fmt.Sprint(claim.TicketNumber)
	}
	controller := agent.Controller{Store: db, Workspace: workspaceManager, Runtime: runtime, GatewayURL: *gatewayURL, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts}
	candidate, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: *source, Branch: *branch, Prompt: *prompt, Publication: store.CandidatePublication{Repository: *repository, Branch: *branch, ExpectedRemoteHead: *expectedHead, ExpectRemoteAbsent: *expectAbsent, Title: claim.TicketTitle}})
	if err != nil {
		fail(err)
	}
	encoded, _ := json.MarshalIndent(candidate, "", "  ")
	fmt.Println(string(encoded))
}

func syncReviewFeedback(ctx context.Context, db *store.Store, client *github.Client, isolator worker.ContainerIsolator, repository, versionID string, ticketID int64, manual string) error {
	var feedback []store.ReviewFeedback
	delivery, err := db.TicketDelivery(ctx, versionID, ticketID)
	if err == nil {
		terminal, err := (github.DeliveredReconciler{Store: db, Client: client, Isolator: isolator}).ReconcileTicket(ctx, delivery)
		if err != nil {
			return err
		}
		if terminal {
			return store.ErrNotReady
		}
		events, err := client.ActionablePullRequestFeedback(ctx, repository, delivery.PullRequestNumber)
		if err != nil {
			return err
		}
		for _, event := range events {
			feedback = append(feedback, store.ReviewFeedback{Source: event.Source, EventID: event.EventID, Author: event.Author, Body: event.Body, BatchID: event.BatchID, Debounce: event.Debounce})
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if strings.TrimSpace(manual) != "" {
		digest := sha256.Sum256([]byte(strings.TrimSpace(manual)))
		feedback = append(feedback, store.ReviewFeedback{Source: "manual", EventID: fmt.Sprintf("%x", digest), Author: "operator", Body: manual})
	}
	if len(feedback) == 0 {
		return nil
	}
	_, err = db.RecordReviewFeedback(ctx, versionID, ticketID, feedback, time.Now().UTC())
	return err
}

func acquireTicketClaim(ctx context.Context, db *store.Store, versionID string, ticketID int64, maxAttempts int, now time.Time, provisionSession ...store.SessionProvisioner) (store.TicketClaim, string, error) {
	claim, err := db.CurrentClaim(ctx, versionID, ticketID)
	if err == nil {
		revisionPrompt, promptErr := db.ClaimedReviewRevisionPrompt(ctx, claim.RunID)
		if promptErr == nil {
			return claim, revisionPrompt, nil
		}
		if !errors.Is(promptErr, store.ErrNotFound) {
			return store.TicketClaim{}, "", promptErr
		}
		return claim, "", nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.TicketClaim{}, "", err
	}
	revision, revisionPrompt, revisionErr := db.ClaimQueuedReviewRevision(ctx, versionID, ticketID, 30*time.Minute, now, 1, maxAttempts, provisionSession...)
	if revisionErr == nil {
		return revision, revisionPrompt, nil
	}
	if errors.Is(revisionErr, store.ErrNeedsAttention) {
		return store.TicketClaim{}, "", revisionErr
	}
	owner, ownerErr := db.RecoveryOwner(ctx, versionID, ticketID)
	if ownerErr != nil {
		return store.TicketClaim{}, "", ownerErr
	}
	replacement, replacementErr := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID:        versionID,
		TicketID:         ticketID,
		Owner:            owner,
		MaxParallelRuns:  1,
		MaxAttempts:      maxAttempts,
		LeaseTTL:         30 * time.Minute,
		Now:              now,
		ProvisionSession: firstProvisioner(provisionSession),
	})
	if replacementErr != nil {
		return store.TicketClaim{}, "", replacementErr
	}
	return replacement, "", nil
}

func firstProvisioner(provisioners []store.SessionProvisioner) store.SessionProvisioner {
	if len(provisioners) == 0 {
		return nil
	}
	return provisioners[0]
}

func runReconcileDelivered(args []string) {
	flags := flag.NewFlagSet("reconcile-delivered", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" {
		fmt.Fprintln(os.Stderr, "reconcile-delivered requires repository")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	db, err := store.Open(ctx, *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	client, err := admittedControlPlaneGitHubClient(ctx, db, config, *githubURL, *repository)
	if err != nil {
		fail(err)
	}
	runtime := worker.DockerRuntime{ControlPlaneID: controlPlaneContainerID(*databasePath)}
	marked, err := (github.DeliveredReconciler{Store: db, Client: client, Isolator: runtime}).Reconcile(ctx, *repository)
	if err != nil {
		fail(persistGitHubAppAdmissionError(ctx, db, err, time.Now().UTC()))
	}
	fmt.Println(marked)
}

func runPollGitHub(args []string) {
	flags := flag.NewFlagSet("poll-github", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	rootNumber := flags.Int64("root", 0, "approved plan root issue number")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	source := flags.String("source", "", "absolute local repository path for review revisions")
	workspaceRoot := flags.String("workspace-root", "", "absolute Ticket Workspace root")
	stateRoot := flags.String("state-root", "", "absolute Codex state root")
	codexAuthFile := flags.String("codex-auth-file", defaultCodexAuthFile(), "absolute host ChatGPT auth.json used to seed Ticket Sessions")
	workspaceRetention := flags.Duration("workspace-retention", 7*24*time.Hour, "retention period before closed Ticket Workspaces are reclaimed")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	gatewayControlURLOverride := flags.String("gateway-control-url", "", "optional host-side Gateway URL; defaults to gateway-url")
	gatewayControlToken := flags.String("gateway-control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	once := flags.Bool("once", false, "perform one durable reconciliation pass")
	interval := flags.Duration("interval", time.Minute, "continuous polling interval")
	maxParallelRuns := flags.Int("max-parallel-runs", 1, "maximum concurrent Worker Runs")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *codexAuthFile == "" || *gatewayURL == "" || *gatewayControlToken == "" || *interval <= 0 || *maxParallelRuns <= 0 || *workspaceRetention <= 0 {
		fmt.Fprintln(os.Stderr, "poll-github requires repository, approved plan root, workspace and ChatGPT authentication configuration, Gateway URL and control credential, positive interval, and positive parallelism")
		os.Exit(2)
	}
	lock, err := startup.AcquireLock(*databasePath)
	if err != nil {
		fail(err)
	}
	defer lock.Close()
	db, err := store.OpenForStartup(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	if err := db.IntegrityCheck(context.Background()); err != nil {
		fail(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		fail(err)
	}
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	provider := &verifiedGitHubAppTokenSource{Database: db, Config: config, APIBase: *githubURL}
	workspaceManager := agent.WorkspaceManager{
		RootDir: *workspaceRoot, CodexStateRoot: *stateRoot, CodexAuthFile: *codexAuthFile,
		RefreshDeliverySource: deliverySourceRefresher(db, provider, *githubURL, *repository),
	}
	runtime := worker.DockerRuntime{DiskPath: *workspaceRoot, ControlPlaneID: controlPlaneContainerID(*databasePath)}
	if reason, err := runtime.Inspect(context.Background()); err != nil {
		fail(err)
	} else if reason != "" {
		fail(errors.New(reason))
	}
	if _, err := workspaceManager.ReclaimClosed(context.Background(), db, *workspaceRetention, time.Now().UTC()); err != nil {
		fail(err)
	}
	var workers sync.WaitGroup
	var workerError error
	var workerErrorMu sync.Mutex
	launch := func(ctx context.Context, claim store.TicketClaim, prompt, branch, expectedHead string, expectAbsent bool) error {
		prompt, err := resolveWorkerPrompt(ctx, db, claim, prompt)
		if err != nil {
			return err
		}
		workerCtx := context.WithoutCancel(ctx)
		run := func() {
			err := runClaimWorker(workerCtx, db, runtime, *repository, *source, workspaceManager, *gatewayURL, config.Runtime.MaxWorkerAttempts, claim, prompt, branch, expectedHead, expectAbsent)
			if err != nil {
				fmt.Fprintln(os.Stderr, "workflow worker:", err)
				if *once {
					workerErrorMu.Lock()
					workerError = errors.Join(workerError, err)
					workerErrorMu.Unlock()
				}
			}
		}
		if *once {
			workers.Add(1)
			go func() {
				defer workers.Done()
				run()
			}()
			return nil
		}
		go run()
		return nil
	}
	launcher := func(ctx context.Context, claim store.TicketClaim, prompt string) error {
		deliveryState, err := db.TicketDelivery(ctx, claim.VersionID, claim.TicketID)
		if err != nil {
			return err
		}
		expectedHead := deliveryState.RemoteHead
		if expectedHead == "" {
			expectedHead = deliveryState.CandidateCommit
		}
		return launch(ctx, claim, prompt, deliveryState.Branch, expectedHead, false)
	}
	launchDelivery := func(ctx context.Context, claim store.TicketClaim) error {
		controller := agent.Controller{Store: db, Workspace: workspaceManager, Runtime: runtime, GatewayURL: *gatewayURL, SourceRepository: *source, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts}
		return controller.RetryDelivery(ctx, claim)
	}
	var lastPollResult github.PollResult
	poll := func() (resultErr error) {
		defer func() {
			resultErr = github.ClassifyPollError(resultErr)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		projector := gatewayControlProjector(*gatewayURL, *gatewayControlURLOverride, *gatewayControlToken)
		poller := github.Poller{Store: db, InboxProjector: projector, MaxFailures: config.Runtime.MaxWorkerAttempts, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts, MaxParallelRuns: *maxParallelRuns, ProvisionSession: workspaceManager.ProvisionCodexSession, ContainerIsolator: runtime}
		leasedCtx, releasePollLease, err := poller.AcquireLease(ctx, *repository)
		if err != nil {
			if errors.Is(err, store.ErrNotReady) {
				return nil
			}
			return err
		}
		ctx = leasedCtx
		defer func() {
			resultErr = errors.Join(resultErr, releasePollLease())
		}()
		if err := poller.PrepareAdmission(ctx, *repository); err != nil {
			return err
		}
		var client *github.Client
		_, err = admitPollGitHubCredential(ctx, poller, provider, *repository, func(token string) error {
			client = github.NewClient(*githubURL, token, nil).WithRepositoryOwner(config.GitHub.Credential.Owner)
			return requireOwnerGuardedControlPlaneRepository(ctx, client, *repository)
		})
		if err != nil {
			if errors.Is(err, store.ErrNotReady) {
				return nil
			}
			err = recordPollAdmissionFailure(ctx, poller, *repository, err)
			fmt.Fprintln(os.Stderr, err)
			return err
		}
		poller.Client = client
		poller.LaunchReview = launcher
		attemptedPlanVersionID := ""
		attemptedPlanAlreadyComplete := false
		bootstrap := func(ctx context.Context) error {
			activeRoot, err := db.SchedulerRoot(ctx, *repository, *rootNumber, time.Now().UTC())
			var isolation *store.DeliveryIsolationRequired
			if errors.As(err, &isolation) {
				fenced, fenceErr := deliveryisolation.DeliveryControllers(ctx, db, runtime, isolation.Targets)
				if fenceErr != nil {
					return errors.Join(err, fenceErr)
				}
				activeRoot, err = db.SchedulerRoot(ctx, *repository, *rootNumber, time.Now().UTC(), fenced...)
			}
			if err != nil {
				return err
			}
			activator := plan.Activator{Reader: client, Projector: projector, Store: db}
			version, err := activator.Activate(ctx, *repository, activeRoot)
			attemptedPlanVersionID = version.ID
			attemptedPlanAlreadyComplete = version.State == store.StateCompleted
			if err != nil {
				return err
			}
			return nil
		}
		control := func(ctx context.Context, bootstrapped bool) (github.BootstrapControlResult, error) {
			controlResult := github.BootstrapControlResult{}
			attemptedPlanVersionID = ""
			attemptedPlanAlreadyComplete = false
			var bootstrapErr error
			if !bootstrapped {
				bootstrapErr = bootstrap(ctx)
			}
			controlResult.AttemptedPlanVersionID = attemptedPlanVersionID
			controlResult.AttemptedPlanAlreadyComplete = attemptedPlanAlreadyComplete
			if bootstrapErr != nil {
				return controlResult, bootstrapErr
			}
			dispatcher := scheduler.Dispatcher{Store: db, Reader: client, Projector: projector, MaxParallelRuns: *maxParallelRuns, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts, LeaseTTL: 30 * time.Minute, Recovery: agent.RecoveryInspector{Containers: runtime, Workspace: workspaceManager}, HostPressure: runtime, ProvisionSession: workspaceManager.ProvisionCodexSession}
			paused, err := dispatcher.DispatchPaused(ctx, *repository)
			if err != nil {
				return controlResult, err
			}
			if paused {
				return controlResult, nil
			}
			roots, err := db.ActivePlanRoots(ctx, *repository)
			if err != nil {
				return controlResult, err
			}
			for _, root := range roots {
				if err := dispatcher.Recover(ctx, *repository, root.RootIssueNumber); err != nil {
					return controlResult, err
				}
			}
			if _, err := workspaceManager.ReclaimClosed(ctx, db, *workspaceRetention, time.Now().UTC()); err != nil {
				return controlResult, err
			}
			var deliveryWorkers *sync.WaitGroup
			if *once {
				deliveryWorkers = &workers
			}
			if err := dispatchPendingDeliveryClaims(ctx, db, *repository, *maxParallelRuns, 30*time.Minute, time.Now().UTC(), launchDelivery, deliveryWorkers, func(err error) {
				workerErrorMu.Lock()
				workerError = errors.Join(workerError, err)
				workerErrorMu.Unlock()
			}); err != nil {
				return controlResult, err
			}
			for {
				claim, claimErr := dispatcher.ClaimNext(ctx, *repository, "workflow-control-plane")
				if claimErr == nil {
					branch := "workflow/ticket-" + fmt.Sprint(claim.TicketNumber)
					if err := launch(ctx, claim, "", branch, "", true); err != nil {
						return controlResult, err
					}
					continue
				}
				if errors.Is(claimErr, store.ErrNoReadyTickets) || errors.Is(claimErr, store.ErrCapacity) || errors.Is(claimErr, store.ErrNotReady) {
					return controlResult, nil
				}
				return controlResult, claimErr
			}
		}
		poller.AfterDelivered = func(ctx context.Context) error {
			_, err := control(ctx, true)
			return err
		}
		result, err := poller.PollWithBootstrap(ctx, *repository, bootstrap, control)
		if shouldLogNeedsAttentionError(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		if err != nil && !errors.Is(err, store.ErrNotReady) && !errors.Is(err, store.ErrNeedsAttention) {
			err = persistGitHubAppAdmissionError(ctx, db, err, time.Now().UTC())
			fmt.Fprintln(os.Stderr, err)
			return err
		}
		if err == nil {
			lastPollResult = result
			encoded, _ := json.Marshal(result)
			fmt.Println(string(encoded))
		}
		return nil
	}
	if *once {
		pollErr := poll()
		workers.Wait()
		workerErrorMu.Lock()
		pollErr = errors.Join(pollErr, workerError)
		workerErrorMu.Unlock()
		if pollErr != nil {
			fail(pollErr)
		}
		return
	}
	for {
		if err := poll(); errors.Is(err, github.ErrLocalPollStore) {
			fail(err)
		}
		time.Sleep(nextPollDelay(db, *repository, *interval, lastPollResult, time.Now().UTC()))
	}
}

func resolveWorkerPrompt(ctx context.Context, db *store.Store, claim store.TicketClaim, persistedRevisionPrompt string) (string, error) {
	if persistedRevisionPrompt != "" {
		return persistedRevisionPrompt, nil
	}
	body, err := db.TicketBody(ctx, claim.VersionID, claim.TicketID)
	if err != nil {
		return "", err
	}
	return implementationPrompt(claim, body), nil
}

func implementationPrompt(claim store.TicketClaim, body string) string {
	return fmt.Sprintf(`Implement Executable Ticket #%d: %s

Authoritative ticket specification:
%s

Implement the exact specification and acceptance criteria above. The Worker intentionally has no GitHub credential. Do not call GitHub or attempt to retrieve the Executable Ticket from GitHub. Commit all changes and leave the Ticket Workspace clean. In the structured Candidate response, commit must be the %s only, with no subject or other text.`, claim.TicketNumber, claim.TicketTitle, body, candidateoutput.CommitSHARequirement)
}

func shouldLogNeedsAttentionError(err error) bool {
	return errors.Is(err, store.ErrNeedsAttention) && strings.Contains(err.Error(), "workflow recover-inbox-delivery")
}

func gatewayControlURL(workerURL, controlURL string) string {
	if controlURL = strings.TrimSpace(controlURL); controlURL != "" {
		return controlURL
	}
	return strings.TrimSpace(workerURL)
}

func gatewayControlProjector(workerURL, controlURL, controlToken string) delivery.HTTPProjector {
	return delivery.HTTPProjector{URL: gatewayControlURL(workerURL, controlURL), ControlPlaneToken: controlToken}
}

func nextPollDelay(db *store.Store, repository string, interval time.Duration, result github.PollResult, now time.Time) time.Duration {
	if cursor, err := db.GitHubPollCursor(context.Background(), repository); err == nil && cursor.NextAttemptAt.After(now) {
		return cursor.NextAttemptAt.Sub(now)
	}
	if result.Feedback > 0 || result.Checks > 0 {
		if delay := interval / 4; delay >= time.Second {
			return delay
		}
		return time.Second
	}
	return interval
}

func runClaimWorker(ctx context.Context, db *store.Store, runtime worker.Runtime, repository, source string, workspaceManager agent.WorkspaceManager, gatewayURL string, maxWorkerAttempts int, claim store.TicketClaim, prompt, branch, expectedHead string, expectAbsent bool) error {
	controller := agent.Controller{Store: db, Workspace: workspaceManager, Runtime: runtime, GatewayURL: gatewayURL, MaxWorkerAttempts: maxWorkerAttempts}
	_, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: source, Branch: branch, Prompt: prompt, Publication: store.CandidatePublication{Repository: repository, Branch: branch, ExpectedRemoteHead: expectedHead, ExpectRemoteAbsent: expectAbsent, Title: claim.TicketTitle}})
	return err
}

func dispatchPendingDeliveryClaims(ctx context.Context, db *store.Store, repository string, maxParallelRuns int, leaseTTL time.Duration, now time.Time, launch func(context.Context, store.TicketClaim) error, workers *sync.WaitGroup, observe func(error)) error {
	if _, err := db.ClaimPendingDeliveryClaims(ctx, repository, maxParallelRuns, leaseTTL, now); err != nil {
		return err
	}
	claims, err := db.PendingDeliveryClaims(ctx, repository, now)
	if err != nil {
		return err
	}
	return launchDeliveryClaims(ctx, claims, launch, workers, observe)
}

func launchDeliveryClaims(ctx context.Context, claims []store.TicketClaim, launch func(context.Context, store.TicketClaim) error, workers *sync.WaitGroup, observe func(error)) error {
	for _, claim := range claims {
		claim := claim
		if workers != nil {
			workers.Add(1)
		}
		go func() {
			if workers != nil {
				defer workers.Done()
			}
			if err := launch(context.WithoutCancel(ctx), claim); err != nil {
				fmt.Fprintln(os.Stderr, "workflow recovered delivery:", err)
				if observe != nil {
					observe(err)
				}
			}
		}()
	}
	return nil
}

func runAnswerInbox(args []string) {
	flags := flag.NewFlagSet("answer-inbox", flag.ExitOnError)
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	questionID := flags.String("question", "", "stable Workflow Inbox question ID")
	answer := flags.String("answer", "", "human decision")
	_ = flags.Parse(args)
	if *repository == "" || *questionID == "" || *answer == "" {
		fmt.Fprintln(os.Stderr, "answer-inbox requires repository, question, and answer")
		os.Exit(2)
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	ctx := context.Background()
	runtime := worker.DockerRuntime{ControlPlaneID: controlPlaneContainerID(*databasePath)}
	if err := answerWorkflowInboxQuestion(ctx, db, runtime, *repository, *questionID, *answer, time.Now().UTC()); err != nil {
		fail(err)
	}
}

type workflowInboxAnswerStore interface {
	deliveryisolation.Store
	AnswerWorkflowQuestionAndQueueInboxProjection(context.Context, string, string, string, time.Time, ...store.DeliveryIsolationProof) (store.DeliveryOutbox, error)
}

func answerWorkflowInboxQuestion(ctx context.Context, db workflowInboxAnswerStore, isolator worker.ContainerIsolator, repository, questionID, answer string, now time.Time) error {
	_, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, repository, questionID, answer, now)
	var isolation *store.DeliveryIsolationRequired
	if !errors.As(err, &isolation) {
		return err
	}
	if isolator == nil {
		return errors.Join(err, errors.New("answer-inbox cannot isolate an active Delivery Controller"))
	}
	fenced, fenceErr := deliveryisolation.DeliveryControllers(ctx, db, isolator, isolation.Targets)
	if fenceErr != nil {
		return errors.Join(err, fenceErr)
	}
	_, err = db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, repository, questionID, answer, now, fenced...)
	return err
}

func runRecoverInboxDelivery(args []string) {
	flags := flag.NewFlagSet("recover-inbox-delivery", flag.ExitOnError)
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	deliveryKey := flags.String("delivery", "", "rejected uncertain Workflow Inbox delivery key")
	questionID := flags.String("question", "", "stable Workflow Inbox recovery question ID")
	answer := flags.String("answer", "", "human recovery authorization")
	_ = flags.Parse(args)
	if *repository == "" {
		fmt.Fprintln(os.Stderr, "recover-inbox-delivery requires repository")
		os.Exit(2)
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	if *deliveryKey == "" {
		keys, err := db.RecoverableUncertainInboxDeliveryKeys(context.Background(), *repository)
		if err != nil {
			fail(err)
		}
		if len(keys) == 0 {
			fail(fmt.Errorf("%w: no rejected uncertain Workflow Inbox deliveries for %s", store.ErrNotFound, *repository))
		}
		for _, key := range keys {
			questionID, err := db.UncertainInboxDeliveryRecoveryQuestionID(context.Background(), *repository, key)
			if err != nil {
				fail(err)
			}
			fmt.Fprintln(os.Stdout, key, questionID)
		}
		return
	}
	if *questionID == "" || *answer == "" {
		fmt.Fprintln(os.Stderr, "recover-inbox-delivery requires question and answer when delivery is provided")
		os.Exit(2)
	}
	if _, err := db.RecoverUncertainInboxDelivery(context.Background(), *repository, *deliveryKey, *questionID, *answer, time.Now().UTC()); err != nil {
		fail(err)
	}
}

func requireOwnerGuardedControlPlaneRepository(ctx context.Context, client *github.Client, repository string) error {
	if err := client.RequireOwnerGuardedRepository(ctx, repository); err != nil {
		return fmt.Errorf("control-plane repository admission: %w", err)
	}
	return nil
}

func admittedControlPlaneGitHubClient(ctx context.Context, database *store.Store, config doctor.Config, apiBase, repository string) (*github.Client, error) {
	client, _, err := admittedControlPlaneGitHubClientAndProvider(ctx, database, config, apiBase, repository)
	return client, err
}

func admittedControlPlaneGitHubClientAndProvider(ctx context.Context, database *store.Store, config doctor.Config, apiBase, repository string) (*github.Client, githubTokenProvider, error) {
	provider := &verifiedGitHubAppTokenSource{Database: database, Config: config, APIBase: apiBase}
	var client *github.Client
	_, err := admitControlPlaneGitHubApp(ctx, database, provider, func(token string) error {
		client = github.NewClient(apiBase, token, nil).WithRepositoryOwner(config.GitHub.Credential.Owner)
		return requireOwnerGuardedControlPlaneRepository(ctx, client, repository)
	})
	return client, provider, err
}

func deliverySourceRefresher(database *store.Store, provider githubTokenProvider, apiBase, repository string) func(context.Context, string) (string, error) {
	return func(ctx context.Context, snapshotPath string) (string, error) {
		token, err := provider.Token(ctx)
		if err != nil {
			return "", persistGitHubAppAdmissionError(ctx, database, err, time.Now().UTC())
		}
		defaultBranch, err := github.NewClient(apiBase, token, nil).DefaultBranchHead(ctx, repository)
		if err != nil {
			return "", persistGitHubAppAdmissionError(ctx, database, err, time.Now().UTC())
		}
		headRef := "refs/heads/" + defaultBranch.Name
		err = (github.DeliverySourceFetcher{Repository: repository, Token: token, APIBase: apiBase}).Fetch(ctx, snapshotPath, headRef)
		if err := persistGitHubAppAdmissionError(ctx, database, err, time.Now().UTC()); err != nil {
			return "", err
		}
		return headRef, nil
	}
}

type restoreFencedGateway struct {
	databasePath    string
	config          doctor.Config
	githubURL       string
	pushURL         string
	dispatcherToken string
	provider        *verifiedGitHubAppTokenSource
	mu              sync.Mutex
}

func newRestoreFencedGateway(databasePath string, config doctor.Config, githubURL, pushURL string) (*restoreFencedGateway, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	return &restoreFencedGateway{
		databasePath:    databasePath,
		config:          config,
		githubURL:       githubURL,
		pushURL:         pushURL,
		dispatcherToken: "gateway-dispatcher-" + hex.EncodeToString(bytes),
		provider:        &verifiedGitHubAppTokenSource{Config: config, APIBase: githubURL},
	}, nil
}

func (g *restoreFencedGateway) withGateway(ctx context.Context, action func(*store.Store, delivery.Gateway, func(context.Context) (string, error)) error) (resultErr error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	db, err := store.Open(ctx, g.databasePath)
	if err != nil {
		return err
	}
	g.provider.Database = db
	defer func() {
		g.provider.Database = nil
		resultErr = errors.Join(resultErr, db.Close())
	}()
	credentialSource := func(ctx context.Context) (string, error) {
		return admitControlPlaneGitHubApp(ctx, db, g.provider, nil)
	}
	remote := &github.DeliveryRemote{
		Client: github.NewClient(g.githubURL, "", nil).WithRepositoryOwner(g.config.GitHub.Credential.Owner),
		Store:  db, PushURL: g.pushURL, CredentialSource: credentialSource,
	}
	gateway := delivery.Gateway{Store: db, Remote: remote, DispatcherToken: g.dispatcherToken}
	return action(db, gateway, credentialSource)
}

func (g *restoreFencedGateway) Deliver(ctx context.Context, request store.DeliveryRequest) (outbox store.DeliveryOutbox, resultErr error) {
	resultErr = g.withGateway(ctx, func(_ *store.Store, gateway delivery.Gateway, _ func(context.Context) (string, error)) error {
		var err error
		outbox, err = gateway.Deliver(ctx, request)
		return err
	})
	return outbox, resultErr
}

func (g *restoreFencedGateway) Initialize(ctx context.Context) error {
	return g.withGateway(ctx, func(db *store.Store, gateway delivery.Gateway, credentialSource func(context.Context) (string, error)) error {
		if _, err := credentialSource(ctx); shouldPauseGatewayForCredential(err) {
			if pauseErr := db.PauseGatewayWrites(ctx, store.ControlPlaneGitHubAppRecoveryRemediation, time.Now().UTC()); pauseErr != nil {
				return pauseErr
			}
		}
		return gateway.QueueGatewayCredentialInboxProjections(ctx)
	})
}

func (g *restoreFencedGateway) QueueGatewayCredentialInboxProjections(ctx context.Context) error {
	return g.withGateway(ctx, func(_ *store.Store, gateway delivery.Gateway, _ func(context.Context) (string, error)) error {
		return gateway.QueueGatewayCredentialInboxProjections(ctx)
	})
}

func (g *restoreFencedGateway) DispatchPending(ctx context.Context, limit int) error {
	return g.withGateway(ctx, func(_ *store.Store, gateway delivery.Gateway, _ func(context.Context) (string, error)) error {
		return gateway.DispatchPending(ctx, limit)
	})
}

func runGateway(args []string) {
	flags := flag.NewFlagSet("gateway", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	listen := flags.String("listen", "", "Gateway listen address")
	controlToken := flags.String("control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	pushURL := flags.String("push-url", "", "optional HTTPS Git push URL")
	outboxInterval := flags.Duration("outbox-interval", time.Second, "durable outbox recovery interval")
	_ = flags.Parse(args)
	if *listen == "" || *controlToken == "" || *outboxInterval <= 0 {
		fmt.Fprintln(os.Stderr, "gateway requires listen address and control-plane credential")
		os.Exit(2)
	}
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	gateway, err := newRestoreFencedGateway(*databasePath, config, *githubURL, *pushURL)
	if err != nil {
		fail(err)
	}
	if err := gateway.Initialize(context.Background()); err != nil {
		fail(err)
	}
	go func() {
		for {
			if err := gateway.QueueGatewayCredentialInboxProjections(context.Background()); err != nil {
				fmt.Fprintln(os.Stderr, "gateway credential recovery projection:", err)
			}
			if err := gateway.DispatchPending(context.Background(), 32); err != nil {
				fmt.Fprintln(os.Stderr, "gateway outbox recovery:", err)
			}
			time.Sleep(*outboxInterval)
		}
	}()
	server := &http.Server{Addr: *listen, Handler: delivery.HTTPHandler(gateway, delivery.HTTPOptions{ControlPlaneToken: *controlToken})}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}

type verifiedGitHubAppTokenSource struct {
	Database *store.Store
	Config   doctor.Config
	APIBase  string
	Client   *http.Client
	Now      func() time.Time

	mu                            sync.Mutex
	identity                      string
	liveInstallationVerifiedUntil time.Time
	provider                      *githubapp.Provider
}

const liveInstallationVerificationTTL = 5 * time.Minute

func (s *verifiedGitHubAppTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	verification, privateKeyPEM, err := verifiedGitHubAppInputs(ctx, s.Database, s.Config)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	identity := fmt.Sprintf("%d/%d/%s/%s", verification.AppID, verification.InstallationID, verification.FingerprintSHA256, verification.VerifiedAt.UTC().Format(time.RFC3339Nano))
	identityChanged := s.provider == nil || s.identity != identity
	if identityChanged || !now.Before(s.liveInstallationVerifiedUntil) {
		if _, err := githubapp.VerifyInstallation(ctx, githubapp.DiscoveryConfig{
			AppID: verification.AppID, PrivateKeyPEM: privateKeyPEM, Owner: s.Config.GitHub.Credential.Owner,
			Repository: s.Config.GitHub.TestRepository, APIBase: s.APIBase, Client: s.Client, Now: s.Now,
		}, verification.InstallationID); err != nil {
			return "", err
		}
		s.liveInstallationVerifiedUntil = now.Add(liveInstallationVerificationTTL)
	}
	if identityChanged {
		s.provider, err = githubapp.NewProvider(githubapp.Config{
			AppID: verification.AppID, InstallationID: verification.InstallationID, PrivateKeyPEM: privateKeyPEM,
			RequiredPermissions: s.Config.GitHub.Credential.Permissions, APIBase: s.APIBase, Client: s.Client, Now: s.Now,
		})
		if err != nil {
			return "", fmt.Errorf("%w: load verified GitHub App: %v", delivery.ErrGatewayCredentialRejected, err)
		}
		s.identity = identity
	}
	return s.provider.Token(ctx)
}

func loadVerifiedGitHubAppProvider(ctx context.Context, database *store.Store, config doctor.Config, apiBase string, client *http.Client) (*githubapp.Provider, store.GitHubAppVerification, []byte, error) {
	verification, privateKeyPEM, err := verifiedGitHubAppInputs(ctx, database, config)
	if err != nil {
		return nil, store.GitHubAppVerification{}, nil, err
	}
	if _, err := githubapp.VerifyInstallation(ctx, githubapp.DiscoveryConfig{
		AppID: verification.AppID, PrivateKeyPEM: privateKeyPEM, Owner: config.GitHub.Credential.Owner,
		Repository: config.GitHub.TestRepository, APIBase: apiBase, Client: client,
	}, verification.InstallationID); err != nil {
		return nil, store.GitHubAppVerification{}, nil, err
	}
	provider, err := githubapp.NewProvider(githubapp.Config{
		AppID: verification.AppID, InstallationID: verification.InstallationID, PrivateKeyPEM: privateKeyPEM,
		RequiredPermissions: config.GitHub.Credential.Permissions, APIBase: apiBase, Client: client,
	})
	if err != nil {
		return nil, store.GitHubAppVerification{}, nil, fmt.Errorf("%w: load verified GitHub App: %v", delivery.ErrGatewayCredentialRejected, err)
	}
	return provider, verification, privateKeyPEM, nil
}

func verifiedGitHubAppInputs(ctx context.Context, database *store.Store, config doctor.Config) (store.GitHubAppVerification, []byte, error) {
	verification, err := database.GitHubAppVerification(ctx)
	if err != nil {
		return store.GitHubAppVerification{}, nil, githubAppVerificationError(err)
	}
	privateKeyPEM, err := os.ReadFile(config.GitHub.Credential.PrivateKeyFile)
	if err != nil {
		return store.GitHubAppVerification{}, nil, fmt.Errorf("%w: read Control Plane GitHub App private key: %v", delivery.ErrGatewayCredentialRejected, err)
	}
	if privateKeyFingerprint(privateKeyPEM) != verification.FingerprintSHA256 {
		return store.GitHubAppVerification{}, nil, fmt.Errorf("%w: GitHub App private key differs from the verified key", delivery.ErrGatewayCredentialRejected)
	}
	if !strings.EqualFold(verification.Owner, config.GitHub.Credential.Owner) || !strings.EqualFold(verification.IntegrationRepository, config.GitHub.TestRepository) {
		return store.GitHubAppVerification{}, nil, fmt.Errorf("%w: GitHub App verification does not match the configured owner and integration repository", delivery.ErrGatewayCredentialRejected)
	}
	return verification, privateKeyPEM, nil
}

func githubAppVerificationError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: Control Plane GitHub App has no persisted verification", delivery.ErrGatewayCredentialRejected)
	}
	return githubAppVerificationStoreError{err: fmt.Errorf("read GitHub App verification: %w", err)}
}

type githubAppVerificationStoreError struct {
	err error
}

func (e githubAppVerificationStoreError) Error() string          { return e.err.Error() }
func (e githubAppVerificationStoreError) Unwrap() error          { return e.err }
func (e githubAppVerificationStoreError) PollStoreFailure() bool { return true }

func shouldPauseGatewayForCredential(err error) bool {
	return errors.Is(err, delivery.ErrGatewayCredentialRejected) || errors.Is(err, githubapp.ErrCredentialUnavailable)
}

func recordPollAdmissionFailure(ctx context.Context, poller github.Poller, repository string, admissionErr error) error {
	_, err := poller.RecordAdmissionFailure(ctx, repository, admissionErr)
	return err
}

func admitPollGitHubCredential(ctx context.Context, poller github.Poller, provider githubTokenProvider, repository string, authenticate func(string) error) (string, error) {
	if err := poller.Ready(ctx, repository); err != nil {
		return "", err
	}
	if provider == nil {
		return "", errors.New("GitHub App token provider is unavailable")
	}
	token, err := provider.Token(ctx)
	if err == nil && authenticate != nil {
		err = authenticate(token)
	}
	if err != nil {
		return "", normalizeGitHubAppCredentialError(err)
	}
	return token, nil
}

func persistGitHubAppPause(ctx context.Context, database *store.Store, credentialErr error, now time.Time) error {
	if !shouldPauseGatewayForCredential(credentialErr) {
		return credentialErr
	}
	if err := database.PauseGatewayWrites(ctx, store.ControlPlaneGitHubAppRecoveryRemediation, now); err != nil {
		return errors.Join(credentialErr, fmt.Errorf("persist Control Plane GitHub App pause: %w", err))
	}
	return credentialErr
}

func admitControlPlaneGitHubApp(ctx context.Context, database *store.Store, provider githubTokenProvider, authenticate func(string) error) (string, error) {
	token, err := provider.Token(ctx)
	if err == nil && authenticate != nil {
		err = authenticate(token)
	}
	if err != nil {
		return "", persistGitHubAppAdmissionError(ctx, database, err, time.Now().UTC())
	}
	return token, nil
}

func persistGitHubAppAdmissionError(ctx context.Context, database *store.Store, admissionErr error, now time.Time) error {
	return persistGitHubAppPause(ctx, database, normalizeGitHubAppCredentialError(admissionErr), now)
}

func normalizeGitHubAppCredentialError(err error) error {
	if err == nil || errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		return err
	}
	if errors.Is(err, githubapp.ErrCredentialUnavailable) {
		return fmt.Errorf("%w: Control Plane GitHub App is unavailable: %w", delivery.ErrGatewayCredentialRejected, err)
	}
	var authenticationFailure interface{ AuthenticationFailure() bool }
	if errors.As(err, &authenticationFailure) && authenticationFailure.AuthenticationFailure() {
		return fmt.Errorf("%w: GitHub rejected the Control Plane GitHub App credential: %w", delivery.ErrGatewayCredentialRejected, err)
	}
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
