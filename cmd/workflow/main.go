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

	"github.com/skyhuang233/workflow/internal/admission"
	"github.com/skyhuang233/workflow/internal/agent"
	candidateoutput "github.com/skyhuang233/workflow/internal/candidate"
	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/deliverysource"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	workerisolation "github.com/skyhuang233/workflow/internal/isolation"
	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/scheduler"
	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
	"github.com/skyhuang233/workflow/internal/workflowbundle"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

const (
	doctorVerificationTimeout = 10 * time.Minute
)

// Version is "dev" for source builds. Immutable Workflow Release builds set
// this exact variable with -ldflags "-X main.Version=<manifest-version>".
var Version = "dev"

func defaultCodexAuthFile() string {
	return strings.TrimSpace(os.Getenv(codexauth.SourceOverrideEnvironment))
}

func resolveDoctorCodexAuth(ctx context.Context, requested string, resolve func(context.Context) (string, error)) (string, error) {
	resolved, err := resolve(ctx)
	if err != nil {
		return "", err
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		same, compareErr := workflowhome.SameFilesystemPath(requested, resolved)
		if compareErr != nil || !same {
			return "", errors.New("--codex-auth-file must match the ChatGPT source verified by codex doctor and codex login status")
		}
	}
	return resolved, nil
}

func controlPlaneContainerID(databasePath string) string {
	canonical, err := startup.DatabaseIdentity(databasePath)
	if err != nil || canonical == "" {
		canonical = filepath.Clean(databasePath)
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

// activeCommandDatabase resolves the only implicit database allowed to a
// normal versioned command. Dispatcher supplies the identity variables, but
// the child derives the path again from active.json and rejects spoofed or
// stale values. Direct/source callers may use an explicit absolute advanced
// override; there is deliberately no cwd workflow.db fallback.
func activeCommandDatabase(override string) (string, error) {
	override = strings.TrimSpace(override)
	if override != "" && !filepath.IsAbs(override) {
		return "", errors.New("--database override must be absolute")
	}
	home := strings.TrimSpace(os.Getenv("WORKFLOW_ACTIVE_HOME"))
	if home == "" {
		home = strings.TrimSpace(os.Getenv("WORKFLOW_HOME"))
	}
	boundHome := home != ""
	if override != "" && !boundHome {
		return filepath.Clean(override), nil
	}
	// A Dispatcher- or Control-Plane-launched child is bound to a Home even
	// with an explicit advanced override: validate that its executable belongs
	// to active.json before allowing any SQLite open.
	if override != "" {
		authoritative, err := activeCommandDatabaseForHome(home)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(filepath.Clean(override), authoritative) {
			return "", errors.New("dispatcher-bound --database must equal the active generation database")
		}
		return filepath.Clean(override), nil
	}
	return activeCommandDatabaseForHome(home)
}

func activeCommandDatabaseForHome(home string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", errors.New("runtime command requires --database or a Workflow Home active generation")
	}
	layout, err := workflowhome.Resolve(home)
	if err != nil {
		return "", err
	}
	active, err := launcher.ReadActive(layout.Root)
	if err != nil {
		return "", fmt.Errorf("runtime command requires an authoritative active generation: %w", err)
	}
	if active.Readiness != "ready" {
		return "", errors.New("runtime command requires a ready active generation")
	}
	if expected := strings.TrimSpace(os.Getenv("WORKFLOW_ACTIVE_GENERATION")); expected != "" && expected != active.Generation {
		return "", errors.New("dispatcher active generation differs from active.json")
	}
	if err := validateVersionedActiveExecutable(layout.Root, active); err != nil {
		return "", err
	}
	database := generationDatabasePath(layout.Root, active)
	if expected := strings.TrimSpace(os.Getenv("WORKFLOW_ACTIVE_DATABASE")); expected != "" && !strings.EqualFold(filepath.Clean(expected), database) {
		return "", errors.New("dispatcher active database differs from active.json")
	}
	info, err := os.Stat(database)
	if err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("active generation database is a directory")
		}
		return "", fmt.Errorf("active generation database is unavailable: %w", err)
	}
	return database, nil
}

type githubTokenProvider interface {
	Token(context.Context) (string, error)
}

func main() {
	if err := validateWorkflowBuildVersion(); err != nil {
		fail(err)
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "--version", "version":
		fmt.Fprintln(os.Stdout, "workflow "+Version)
	case "onboarding":
		if err := onboardingCommand(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
	case "github":
		if err := githubCommand(os.Args[2:], os.Stdout); err != nil {
			fail(err)
		}
	case "serve":
		if err := serveCommand(os.Args[2:], os.Stdout); err != nil {
			fail(err)
		}
	case "serve-child":
		if err := serveChildCommand(os.Args[2:]); err != nil {
			fail(err)
		}
	case "status":
		if err := runtimeStatusCommand(os.Args[2:], os.Stdout); err != nil {
			fail(err)
		}
	case "logs":
		if err := runtimeLogsCommand(os.Args[2:], os.Stdout); err != nil {
			fail(err)
		}
	case "stop":
		if err := runtimeStopCommand(os.Args[2:], os.Stdout); err != nil {
			fail(err)
		}
	case "runtime-configure":
		if err := runtimeConfigureCommand(os.Args[2:], os.Stdout); err != nil {
			fail(err)
		}
	case "doctor":
		runDoctor(os.Args[2:])
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
	case "drill-backup":
		runBackupDrill(os.Args[2:])
	case "metrics":
		runMetrics(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func validateWorkflowBuildVersion() error {
	if Version == "dev" {
		return nil
	}
	if err := workflowbundle.ValidateVersion(Version); err != nil {
		return fmt.Errorf("invalid published Workflow CLI version: %w", err)
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  workflow onboarding plan|apply|verify [--workflow-home <absolute>]")
	fmt.Fprintln(os.Stderr, "  workflow github <operation> --repo <absolute> [options]")
	fmt.Fprintln(os.Stderr, "  workflow doctor --workflow-repository owner/repository [--config path] [--database path] [--codex-auth-file path] [--report path]")
	fmt.Fprintln(os.Stderr, "  workflow run-ticket [options]")
	fmt.Fprintln(os.Stderr, "  workflow gateway [options]")
	fmt.Fprintln(os.Stderr, "  workflow poll-github [options]")
	fmt.Fprintln(os.Stderr, "  workflow reconcile-delivered [options]")
	fmt.Fprintln(os.Stderr, "  workflow answer-inbox [options]")
	fmt.Fprintln(os.Stderr, "  workflow recover-inbox-delivery [options]")
	fmt.Fprintln(os.Stderr, "  workflow backup [--database path] [--output path]")
	fmt.Fprintln(os.Stderr, "  workflow drill-backup --backup path")
	fmt.Fprintln(os.Stderr, "  workflow metrics [--database path] [--backup path]")
}

func runBackup(args []string) {
	flags := flag.NewFlagSet("backup", flag.ExitOnError)
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
	outputPath := flags.String("output", "", "online SQLite backup destination")
	_ = flags.Parse(args)
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	if *outputPath == "" {
		*outputPath = *databasePath + ".backup"
	}
	db, err := store.OpenActivated(context.Background(), *databasePath)
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
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
	backupPath := flags.String("backup", "", "verified SQLite online backup")
	_ = flags.Parse(args)
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	if *backupPath == "" {
		*backupPath = *databasePath + ".backup"
	}
	db, err := store.OpenActivated(context.Background(), *databasePath)
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
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
	reportPath := flags.String("report", "", "optional Markdown report path")
	workflowRepository := flags.String("workflow-repository", "", "GitHub repository containing the Worker publisher workflow")
	codexAuthFile := flags.String("codex-auth-file", defaultCodexAuthFile(), "optional controlled override for the ChatGPT source verified through Codex doctor")
	_ = flags.Parse(args)
	if *workflowRepository == "" {
		fmt.Fprintln(os.Stderr, "doctor requires workflow-repository")
		os.Exit(2)
	}
	resolvedDatabase, databaseErr := activeCommandDatabase(*databasePath)
	if databaseErr != nil {
		fmt.Fprintln(os.Stderr, databaseErr)
		os.Exit(1)
	}
	*databasePath = resolvedDatabase
	resolvedCodexAuth, err := resolveDoctorCodexAuth(context.Background(), *codexAuthFile, codexauth.ResolveChatGPT)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	*codexAuthFile = resolvedCodexAuth

	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	database, err := store.OpenActivated(context.Background(), *databasePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()
	provider := &verifiedGitHubPATSource{Database: database, Config: config}
	secret, err := admitControlPlaneGitHubCredential(context.Background(), database, provider, func(token string) error {
		_, verifyErr := (githubcredential.Verifier{}).Verify(context.Background(), token, config.GitHub.Credential.Owner)
		return verifyErr
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	releaseFetcher := doctor.ReleaseFetcher{WorkflowRepository: *workflowRepository}
	manifest, manifestJSON, err := releaseFetcher.Fetch(context.Background(), config, secret)
	if err != nil {
		fmt.Fprintln(os.Stderr, persistGitHubCredentialAdmissionError(context.Background(), database, err, time.Now().UTC()))
		os.Exit(1)
	}
	checks := []doctor.Check{
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
		doctor.WorkerCodexSessionCheck{Executor: doctor.OSExecutor{}, Image: manifest.Worker.Image, AuthFile: *codexAuthFile},
		doctor.SQLiteCheck{Path: *databasePath},
		doctor.DockerCheck{Manifest: manifest},
		doctor.WorkerRegistryCheck{Image: manifest.Worker.Image},
	}
	patVerification, readErr := database.GitHubPATVerification(context.Background())
	if readErr != nil {
		fmt.Fprintln(os.Stderr, readErr)
		os.Exit(1)
	}
	checks = append(checks, doctor.GitHubPATCheck{Pin: config.GitHub.Credential, Token: secret, Verification: patVerification})
	runner := doctor.Runner{Checks: checks, Secrets: []string{secret}}
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
		fmt.Fprintln(os.Stderr, persistGitHubCredentialAdmissionError(context.Background(), database, err, time.Now().UTC()))
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
		fmt.Fprintln(os.Stderr, persistGitHubCredentialAdmissionError(context.Background(), database, err, time.Now().UTC()))
		os.Exit(1)
	}
	if currentManifest != manifest || string(currentManifestJSON) != string(manifestJSON) {
		fmt.Fprintln(os.Stderr, "Workflow Release changed during doctor verification")
		os.Exit(1)
	}
	if err := database.ActivateWorkerReleaseFenced(context.Background(), store.WorkerRelease{
		Version: manifest.Version, SourceCommit: manifest.CandidateSourceCommit,
		ImageReference: manifest.Worker.Image, ManifestJSON: string(manifestJSON),
		VerifiedAt: report.GeneratedAt, ActivatedAt: report.GeneratedAt,
	}, expectedActiveImage); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("activated Worker image %s for new Worker Runs\n", manifest.Worker.Image)
}

func runTicket(args []string) {
	flags := flag.NewFlagSet("run-ticket", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
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
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, err := store.OpenActivated(ctx, *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	client, provider, err := admittedControlPlaneGitHubClientAndProvider(ctx, db, config, *githubURL, *repository)
	if err != nil {
		fail(err)
	}
	workspaceManager := agent.WorkspaceManager{
		RootDir: *workspaceRoot, DeliverySourceRoot: deliverysource.SharedRoot(*workspaceRoot), CodexStateRoot: *stateRoot, CodexAuthFile: *codexAuthFile,
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
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
	repository := flags.String("repository", "", "GitHub owner/repository")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" {
		fmt.Fprintln(os.Stderr, "reconcile-delivered requires repository")
		os.Exit(2)
	}
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	db, err := store.OpenActivated(ctx, *databasePath)
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
		fail(persistGitHubCredentialAdmissionError(ctx, db, err, time.Now().UTC()))
	}
	fmt.Println(marked)
}

func runPollGitHub(args []string) {
	flags := flag.NewFlagSet("poll-github", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
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
	runtimeOwner := flags.String("owner", "", "verified Workflow Home owner (internal runtime mode)")
	runtimeCredentialPath := flags.String("credential-relative-path", `state\credentials\github.pat`, "Workflow Home relative PAT path (internal runtime mode)")
	runtimeMaxWorkerAttempts := flags.Int("max-worker-attempts", 3, "maximum attempts in internal runtime mode")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *codexAuthFile == "" || *gatewayURL == "" || *gatewayControlToken == "" || *interval <= 0 || *maxParallelRuns <= 0 || *workspaceRetention <= 0 {
		fmt.Fprintln(os.Stderr, "poll-github requires repository, approved plan root, workspace and ChatGPT authentication configuration, Gateway URL and control credential, positive interval, and positive parallelism")
		os.Exit(2)
	}
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	lock, err := startup.AcquireLock(*databasePath)
	if err != nil {
		fail(err)
	}
	defer lock.Close()
	db, err := store.OpenActivated(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	if err := db.IntegrityCheck(context.Background()); err != nil {
		fail(err)
	}
	var config doctor.Config
	if *runtimeOwner != "" {
		config.Runtime.MaxWorkerAttempts = *runtimeMaxWorkerAttempts
		config.GitHub.Credential = doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: *runtimeOwner, PlaintextRelativePath: *runtimeCredentialPath}
	} else {
		config, err = doctor.LoadConfig(*configPath)
		if err != nil {
			fail(err)
		}
	}
	provider := &verifiedGitHubPATSource{Database: db, Config: config}
	workspaceManager := agent.WorkspaceManager{
		RootDir: *workspaceRoot, DeliverySourceRoot: deliverysource.SharedRoot(*workspaceRoot), CodexStateRoot: *stateRoot, CodexAuthFile: *codexAuthFile,
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
			var activeRoot int64
			err := workerisolation.RetryWorkerTransition(ctx, db, runtime, func(isolated []store.WorkerIsolationProof) error {
				var err error
				activeRoot, err = db.SchedulerRoot(ctx, *repository, *rootNumber, time.Now().UTC(), isolated...)
				return err
			})
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
			dispatcher := scheduler.Dispatcher{Store: db, Reader: client, Projector: projector, MaxParallelRuns: *maxParallelRuns, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts, LeaseTTL: 30 * time.Minute, Recovery: agent.RecoveryInspector{Containers: runtime, Workspace: workspaceManager}, HostPressure: runtime, ProvisionSession: workspaceManager.ProvisionCodexSession, Admission: admission.Service{Store: db}}
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
			err = persistGitHubCredentialAdmissionError(ctx, db, err, time.Now().UTC())
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
	databasePath := flags.String("database", "", "advanced SQLite control-plane database override")
	workflowHome := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	repository := flags.String("repository", "", "GitHub owner/repository")
	questionID := flags.String("question", "", "stable Workflow Inbox question ID")
	answer := flags.String("answer", "", "human decision")
	_ = flags.Parse(args)
	if *repository == "" || *questionID == "" || *answer == "" {
		fmt.Fprintln(os.Stderr, "answer-inbox requires repository, question, and answer")
		os.Exit(2)
	}
	resolvedDatabase, err := answerInboxDatabasePath(*databasePath, *workflowHome)
	if err != nil {
		fail(err)
	}
	db, err := store.OpenActivated(context.Background(), resolvedDatabase)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	ctx := context.Background()
	runtime := worker.DockerRuntime{ControlPlaneID: controlPlaneContainerID(resolvedDatabase)}
	if err := answerWorkflowInboxQuestion(ctx, db, runtime, *repository, *questionID, *answer, time.Now().UTC()); err != nil {
		fail(err)
	}
}

func answerInboxDatabasePath(databasePath, homeOverride string) (string, error) {
	// A Dispatcher-bound command is never an advanced source invocation: even
	// an explicit absolute path must be the authoritative active generation.
	// This closes the last alternate DB path for inbox decisions.
	if strings.TrimSpace(os.Getenv("WORKFLOW_ACTIVE_HOME")) != "" {
		return activeCommandDatabase(databasePath)
	}
	if strings.TrimSpace(databasePath) != "" {
		if !filepath.IsAbs(databasePath) {
			return "", errors.New("answer-inbox --database override must be absolute")
		}
		return filepath.Clean(databasePath), nil
	}
	if strings.TrimSpace(os.Getenv("WORKFLOW_ACTIVE_HOME")) != "" {
		return activeCommandDatabase("")
	}
	layout, err := workflowhome.Resolve(homeOverride)
	if err != nil {
		return "", err
	}
	active, err := launcher.ReadActive(layout.Root)
	if err != nil {
		return "", fmt.Errorf("answer-inbox requires an authoritative active generation: %w", err)
	}
	if active.Readiness != "ready" {
		return "", errors.New("answer-inbox requires a ready active generation")
	}
	if err := validateVersionedActiveExecutable(layout.Root, active); err != nil {
		return "", err
	}
	path := generationDatabasePath(layout.Root, active)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("generation database is a directory")
		}
		return "", fmt.Errorf("answer-inbox active generation database is unavailable: %w", err)
	}
	return path, nil
}

// validateVersionedActiveExecutable fail-closes a CLI copied from any
// generation other than active.json's generation. Source/test binaries remain
// valid direct callers; only a versioned generation path asserts this identity.
func validateVersionedActiveExecutable(home string, active launcher.Active) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return validateActiveExecutablePath(executable, home, active)
}

func validateActiveExecutablePath(executable, home string, active launcher.Active) error {
	directory := filepath.Clean(filepath.Dir(executable))
	generationsRoot := filepath.Clean(filepath.Join(home, "platform", "generations"))
	relative, err := filepath.Rel(generationsRoot, directory)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return nil
	}
	expected := filepath.Clean(filepath.Join(generationsRoot, active.Generation))
	if directory != expected || !strings.EqualFold(filepath.Base(executable), "workflow.exe") {
		return errors.New("versioned Workflow CLI generation differs from active.json")
	}
	return nil
}

type workflowInboxAnswerStore interface {
	workerisolation.Store
	AnswerWorkflowQuestionAndQueueInboxProjection(context.Context, string, string, string, time.Time, ...store.WorkerIsolationProof) (store.DeliveryOutbox, error)
}

func answerWorkflowInboxQuestion(ctx context.Context, db workflowInboxAnswerStore, isolator worker.ContainerIsolator, repository, questionID, answer string, now time.Time) error {
	return workerisolation.RetryWorkerTransition(ctx, db, isolator, func(isolated []store.WorkerIsolationProof) error {
		_, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, repository, questionID, answer, now, isolated...)
		return err
	})
}

func runRecoverInboxDelivery(args []string) {
	flags := flag.NewFlagSet("recover-inbox-delivery", flag.ExitOnError)
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
	repository := flags.String("repository", "", "GitHub owner/repository")
	deliveryKey := flags.String("delivery", "", "rejected uncertain Workflow Inbox delivery key")
	questionID := flags.String("question", "", "stable Workflow Inbox recovery question ID")
	answer := flags.String("answer", "", "human recovery authorization")
	_ = flags.Parse(args)
	if *repository == "" {
		fmt.Fprintln(os.Stderr, "recover-inbox-delivery requires repository")
		os.Exit(2)
	}
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	db, err := store.OpenActivated(context.Background(), *databasePath)
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
	provider := &verifiedGitHubPATSource{Database: database, Config: config}
	var client *github.Client
	_, err := admitControlPlaneGitHubCredential(ctx, database, provider, func(token string) error {
		client = github.NewClient(apiBase, token, nil).WithRepositoryOwner(config.GitHub.Credential.Owner)
		return requireOwnerGuardedControlPlaneRepository(ctx, client, repository)
	})
	return client, provider, err
}

func deliverySourceRefresher(database *store.Store, provider githubTokenProvider, apiBase, repository string) func(context.Context, string) (string, error) {
	return func(ctx context.Context, snapshotPath string) (string, error) {
		token, err := provider.Token(ctx)
		if err != nil {
			return "", persistGitHubCredentialAdmissionError(ctx, database, err, time.Now().UTC())
		}
		defaultBranch, err := github.NewClient(apiBase, token, nil).DefaultBranchHead(ctx, repository)
		if err != nil {
			return "", persistGitHubCredentialAdmissionError(ctx, database, err, time.Now().UTC())
		}
		headRef := "refs/heads/" + defaultBranch.Name
		err = (github.DeliverySourceFetcher{Repository: repository, Token: token, APIBase: apiBase}).Fetch(ctx, snapshotPath, headRef)
		if err := persistGitHubCredentialAdmissionError(ctx, database, err, time.Now().UTC()); err != nil {
			return "", err
		}
		return headRef, nil
	}
}

type generationGateway struct {
	databasePath    string
	config          doctor.Config
	githubURL       string
	pushURL         string
	dispatcherToken string
	provider        *verifiedGitHubPATSource
	mu              sync.Mutex
}

func newGenerationGateway(databasePath string, config doctor.Config, githubURL, pushURL string) (*generationGateway, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	return &generationGateway{
		databasePath:    databasePath,
		config:          config,
		githubURL:       githubURL,
		pushURL:         pushURL,
		dispatcherToken: "gateway-dispatcher-" + hex.EncodeToString(bytes),
		provider:        &verifiedGitHubPATSource{Config: config},
	}, nil
}

func (g *generationGateway) withGateway(ctx context.Context, action func(*store.Store, delivery.Gateway, func(context.Context) (string, error)) error) (resultErr error) {
	return g.withGatewayStore(ctx, store.OpenActivated, action)
}

func (g *generationGateway) withGatewayStore(ctx context.Context, openStore func(context.Context, string) (*store.Store, error), action func(*store.Store, delivery.Gateway, func(context.Context) (string, error)) error) (resultErr error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	db, err := openStore(ctx, g.databasePath)
	if err != nil {
		return err
	}
	g.provider.Database = db
	defer func() {
		g.provider.Database = nil
		resultErr = errors.Join(resultErr, db.Close())
	}()
	credentialSource := func(ctx context.Context) (string, error) {
		return admitControlPlaneGitHubCredential(ctx, db, g.provider, nil)
	}
	remote := &github.DeliveryRemote{
		Client: github.NewClient(g.githubURL, "", nil).WithRepositoryOwner(g.config.GitHub.Credential.Owner),
		Store:  db, PushURL: g.pushURL, CredentialSource: credentialSource,
	}
	gateway := delivery.Gateway{
		Store: db, Remote: remote, DispatcherToken: g.dispatcherToken,
		WorkerIsolator: worker.DockerRuntime{ControlPlaneID: controlPlaneContainerID(g.databasePath)},
	}
	return action(db, gateway, credentialSource)
}

func (g *generationGateway) Deliver(ctx context.Context, request store.DeliveryRequest) (outbox store.DeliveryOutbox, resultErr error) {
	resultErr = g.withGateway(ctx, func(_ *store.Store, gateway delivery.Gateway, _ func(context.Context) (string, error)) error {
		var err error
		outbox, err = gateway.Deliver(ctx, request)
		return err
	})
	return outbox, resultErr
}

func (g *generationGateway) Initialize(ctx context.Context) error {
	return g.withGatewayStore(ctx, store.OpenActivated, func(db *store.Store, gateway delivery.Gateway, credentialSource func(context.Context) (string, error)) error {
		if _, err := credentialSource(ctx); shouldPauseGatewayForCredential(err) {
			if pauseErr := db.PauseGatewayWrites(ctx, store.ControlPlaneGitHubCredentialRecoveryRemediation, time.Now().UTC()); pauseErr != nil {
				return pauseErr
			}
		}
		return gateway.QueueGatewayCredentialInboxProjections(ctx)
	})
}

func (g *generationGateway) QueueGatewayCredentialInboxProjections(ctx context.Context) error {
	return g.withGateway(ctx, func(_ *store.Store, gateway delivery.Gateway, _ func(context.Context) (string, error)) error {
		return gateway.QueueGatewayCredentialInboxProjections(ctx)
	})
}

func (g *generationGateway) DispatchPending(ctx context.Context, limit int) error {
	return g.withGateway(ctx, func(_ *store.Store, gateway delivery.Gateway, _ func(context.Context) (string, error)) error {
		return gateway.DispatchPending(ctx, limit)
	})
}

func runGateway(args []string) {
	flags := flag.NewFlagSet("gateway", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", "", "advanced absolute SQLite control-plane database override")
	listen := flags.String("listen", "", "Gateway listen address")
	controlToken := flags.String("control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	pushURL := flags.String("push-url", "", "optional HTTPS Git push URL")
	outboxInterval := flags.Duration("outbox-interval", time.Second, "durable outbox recovery interval")
	runtimeOwner := flags.String("owner", "", "verified Workflow Home owner (internal runtime mode)")
	runtimeCredentialPath := flags.String("credential-relative-path", `state\credentials\github.pat`, "Workflow Home relative PAT path (internal runtime mode)")
	_ = flags.Parse(args)
	if *listen == "" || *controlToken == "" || *outboxInterval <= 0 {
		fmt.Fprintln(os.Stderr, "gateway requires listen address and control-plane credential")
		os.Exit(2)
	}
	resolvedDatabase, err := activeCommandDatabase(*databasePath)
	if err != nil {
		fail(err)
	}
	*databasePath = resolvedDatabase
	var config doctor.Config
	if *runtimeOwner != "" {
		config.GitHub.Credential = doctor.GitHubCredentialPin{Kind: "classic-pat", Owner: *runtimeOwner, PlaintextRelativePath: *runtimeCredentialPath}
	} else {
		config, err = doctor.LoadConfig(*configPath)
		if err != nil {
			fail(err)
		}
	}
	gateway, err := newGenerationGateway(*databasePath, config, *githubURL, *pushURL)
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

type verifiedGitHubPATSource struct {
	Database *store.Store
	Config   doctor.Config
}

func (s *verifiedGitHubPATSource) Token(ctx context.Context) (string, error) {
	return verifiedClassicPAT(ctx, s.Database, s.Config)
}

func verifiedClassicPAT(ctx context.Context, database *store.Store, config doctor.Config, expectedCredentialPath ...string) (string, error) {
	if database == nil {
		return "", fmt.Errorf("%w: Control Plane PAT store is unavailable", delivery.ErrGatewayCredentialRejected)
	}
	if len(expectedCredentialPath) > 1 {
		return "", fmt.Errorf("%w: Control Plane PAT credential path is ambiguous", delivery.ErrGatewayCredentialRejected)
	}
	verification, err := database.GitHubPATVerification(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%w: Control Plane PAT has no persisted verification", delivery.ErrGatewayCredentialRejected)
	}
	if err != nil {
		return "", githubCredentialVerificationStoreError{err: fmt.Errorf("read PAT verification: %w", err)}
	}
	path := verification.CredentialPath
	if len(expectedCredentialPath) == 1 {
		path = expectedCredentialPath[0]
		if path == "" || !filepath.IsAbs(path) {
			return "", fmt.Errorf("%w: Control Plane PAT credential path is invalid", delivery.ErrGatewayCredentialRejected)
		}
	}
	token, err := credential.NewFileStore(path).Get(ctx, credential.GatewayTarget)
	if err != nil {
		return "", fmt.Errorf("%w: read Control Plane PAT: %v", delivery.ErrGatewayCredentialRejected, err)
	}
	if credential.Fingerprint(token) != verification.FingerprintSHA256 || !strings.EqualFold(verification.Owner, config.GitHub.Credential.Owner) || verification.Status != "verified" || !strings.EqualFold(filepath.Clean(verification.CredentialPath), filepath.Clean(path)) {
		return "", fmt.Errorf("%w: Control Plane PAT differs from its verified owner-bound record", delivery.ErrGatewayCredentialRejected)
	}
	return token, nil
}

type githubCredentialVerificationStoreError struct {
	err error
}

func (e githubCredentialVerificationStoreError) Error() string          { return e.err.Error() }
func (e githubCredentialVerificationStoreError) Unwrap() error          { return e.err }
func (e githubCredentialVerificationStoreError) PollStoreFailure() bool { return true }

func shouldPauseGatewayForCredential(err error) bool {
	return errors.Is(err, delivery.ErrGatewayCredentialRejected)
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
		return "", errors.New("Control Plane GitHub credential provider is unavailable")
	}
	token, err := provider.Token(ctx)
	if err == nil && authenticate != nil {
		err = authenticate(token)
	}
	if err != nil {
		return "", normalizeGitHubCredentialError(err)
	}
	return token, nil
}

func persistGitHubCredentialPause(ctx context.Context, database *store.Store, credentialErr error, now time.Time) error {
	if !shouldPauseGatewayForCredential(credentialErr) {
		return credentialErr
	}
	if err := database.PauseGatewayWrites(ctx, store.ControlPlaneGitHubCredentialRecoveryRemediation, now); err != nil {
		return errors.Join(credentialErr, fmt.Errorf("persist Control Plane GitHub credential pause: %w", err))
	}
	return credentialErr
}

func admitControlPlaneGitHubCredential(ctx context.Context, database *store.Store, provider githubTokenProvider, authenticate func(string) error) (string, error) {
	token, err := provider.Token(ctx)
	if err == nil && authenticate != nil {
		err = authenticate(token)
	}
	if err != nil {
		return "", persistGitHubCredentialAdmissionError(ctx, database, err, time.Now().UTC())
	}
	return token, nil
}

func persistGitHubCredentialAdmissionError(ctx context.Context, database *store.Store, admissionErr error, now time.Time) error {
	return persistGitHubCredentialPause(ctx, database, normalizeGitHubCredentialError(admissionErr), now)
}

func normalizeGitHubCredentialError(err error) error {
	if err == nil || errors.Is(err, delivery.ErrGatewayCredentialRejected) {
		return err
	}
	var authenticationFailure interface{ AuthenticationFailure() bool }
	if errors.As(err, &authenticationFailure) && authenticationFailure.AuthenticationFailure() {
		return fmt.Errorf("%w: GitHub rejected the Control Plane GitHub PAT: %w", delivery.ErrGatewayCredentialRejected, err)
	}
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
