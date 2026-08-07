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
	"strings"
	"sync"
	"time"

	"github.com/skyhuang233/workflow/internal/agent"
	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/githubcontract"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/scheduler"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
	"golang.org/x/term"
)

const defaultControlPlaneDatabase = "workflow.db"

type admittedCredential string

func (c admittedCredential) Get(context.Context, string) (string, error) {
	return string(c), nil
}

func (admittedCredential) Set(context.Context, string, string) error {
	return errors.New("admitted credentials cannot be replaced")
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
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  workflow doctor --workflow-repository owner/repository [--config path] [--database path] [--report path]")
	fmt.Fprintln(os.Stderr, "  workflow credential provision [--config path] [--database path]")
	fmt.Fprintln(os.Stderr, "  workflow run-ticket [options]")
	fmt.Fprintln(os.Stderr, "  workflow gateway [options]")
	fmt.Fprintln(os.Stderr, "  workflow poll-github [options]")
	fmt.Fprintln(os.Stderr, "  workflow reconcile-delivered [options]")
	fmt.Fprintln(os.Stderr, "  workflow answer-inbox [options]")
	fmt.Fprintln(os.Stderr, "  workflow recover-inbox-delivery [options]")
}

func runDoctor(args []string) {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	reportPath := flags.String("report", "", "optional Markdown report path")
	workflowRepository := flags.String("workflow-repository", "", "GitHub repository containing the Worker publisher workflow")
	_ = flags.Parse(args)
	if *workflowRepository == "" {
		fmt.Fprintln(os.Stderr, "doctor requires workflow-repository")
		os.Exit(2)
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
	verification, verificationErr := database.GatewayCredentialVerification(context.Background())
	if verificationErr != nil && !errors.Is(verificationErr, store.ErrNotFound) {
		fmt.Fprintln(os.Stderr, verificationErr)
		os.Exit(1)
	}
	secret, err := admitGatewayCredential(context.Background(), database, func(token string) error {
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
		fmt.Fprintln(os.Stderr, persistGatewayCredentialAdmissionError(context.Background(), database, err, time.Now().UTC()))
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
		doctor.CommandCheck{
			CheckName: "no-mistakes CLI",
			Executor:  doctor.OSExecutor{},
			Version: doctor.CommandExpectation{
				Command:      []string{"no-mistakes", "--version"},
				Tool:         "no-mistakes",
				ExactVersion: config.NoMistakes.Version,
				ExactCommit:  config.NoMistakes.UpstreamCommit,
			},
		},
		doctor.CodexResumeCheck{Executor: doctor.OSExecutor{}},
		doctor.SQLiteCheck{Path: *databasePath},
		doctor.DockerCheck{Manifest: manifest},
		doctor.WorkerRegistryCheck{Image: manifest.Image},
		doctor.GitHubCredentialCheck{Pin: config.GitHub.Credential, IntegrationRepository: config.GitHub.TestRepository, Credentials: admittedCredentials, Verification: verification},
		doctor.GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes, Credentials: admittedCredentials},
	}, Secrets: []string{secret}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
		fmt.Fprintln(os.Stderr, persistGatewayCredentialAdmissionError(context.Background(), database, err, time.Now().UTC()))
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
		fmt.Fprintln(os.Stderr, persistGatewayCredentialAdmissionError(context.Background(), database, err, time.Now().UTC()))
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
	_ = flags.Parse(os.Args[3:])
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		exitError(err)
	}
	fmt.Fprint(os.Stderr, "Fine-grained PAT (input hidden): ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		exitError(fmt.Errorf("read credential: %w", err))
	}
	token := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(token, "github_pat_") {
		exitError(fmt.Errorf("a fine-grained PAT is required"))
	}
	credentialStore := credential.NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := store.Open(ctx, *databasePath)
	if err != nil {
		exitError(err)
	}
	defer database.Close()
	if err := provisionGatewayCredential(ctx, database, credentialStore, config, token); err != nil {
		exitError(err)
	}
	fmt.Println("Gateway Credential stored in Windows Credential Manager and live write contract verified")
}

func provisionGatewayCredential(ctx context.Context, database *store.Store, credentialStore credential.Store, config doctor.Config, token string) (resultErr error) {
	owner, err := credentialRotationOwner()
	if err != nil {
		return err
	}
	rotation, err := database.BeginGatewayCredentialRotation(ctx, owner, "Gateway Credential rotation is in progress", time.Now().UTC())
	if err != nil {
		return err
	}
	resumed := false
	defer func() {
		if !resumed {
			resultErr = errors.Join(resultErr, database.EndGatewayCredentialRotation(context.Background(), rotation, time.Now().UTC()))
		}
	}()
	if err := database.RecoverExpiredGatewayDeliveryClaims(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("recover expired Gateway delivery claims before credential rotation: %w", err)
	}
	if err := database.WaitForGatewayWritesQuiesced(ctx); err != nil {
		return fmt.Errorf("wait for Gateway writes to finish before credential rotation: %w", err)
	}
	if err := database.RenewGatewayCredentialRotation(ctx, rotation, time.Now().UTC()); err != nil {
		return err
	}
	if err := (githubcontract.Verifier{}).Verify(ctx, token, config.GitHub.Credential.Owner, config.GitHub.TestRepository); err != nil {
		return fmt.Errorf("live contract failed; the existing Gateway Credential was not replaced: %w", err)
	}
	previousToken, previousErr := credentialStore.Get(context.Background(), credential.GatewayTarget)
	if err := credentialStore.Set(context.Background(), credential.GatewayTarget, token); err != nil {
		return fmt.Errorf("replace Gateway Credential; writes remain paused because replacement state is uncertain: %w", err)
	}
	if err := database.RenewGatewayCredentialRotation(ctx, rotation, time.Now().UTC()); err != nil {
		return err
	}
	if err := database.RecordGatewayCredentialVerification(ctx, store.GatewayCredentialVerification{
		FingerprintSHA256:     credential.Fingerprint(token),
		Owner:                 config.GitHub.Credential.Owner,
		IntegrationRepository: config.GitHub.TestRepository,
		VerifiedAt:            time.Now().UTC(),
	}); err != nil {
		if previousErr == nil && previousToken != "" {
			_ = credentialStore.Set(context.Background(), credential.GatewayTarget, previousToken)
		}
		return fmt.Errorf("record verification; Gateway writes remain paused and the prior credential was restored when available: %w", err)
	}
	if err := database.ResumeGatewayWrites(ctx, rotation, time.Now().UTC()); err != nil {
		return err
	}
	resumed = true
	return nil
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
	prompt := flags.String("prompt", "", "Worker prompt")
	reviewFeedback := flags.String("review-feedback", "", "human pull-request feedback to queue for the next revision round")
	branch := flags.String("branch", "", "ticket branch")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	expectedHead := flags.String("expected-remote-head", "", "current remote ticket branch head")
	expectAbsent := flags.Bool("expect-remote-absent", true, "require the ticket branch to be absent")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *ticketID == 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *gatewayURL == "" || (*expectedHead != "") == *expectAbsent {
		fmt.Fprintln(os.Stderr, "run-ticket requires repository, root, ticket-id, source, workspace-root, state-root, Gateway URL, and exactly one remote-head expectation")
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
	var client *github.Client
	_, err = admitGatewayCredential(ctx, db, func(token string) error {
		client = github.NewClient(*githubURL, token, nil).WithRepositoryOwner(config.GitHub.Credential.Owner)
		return requireOwnerGuardedControlPlaneRepository(ctx, client, *repository)
	})
	if err != nil {
		fail(err)
	}
	snapshot, err := client.ReadPlan(ctx, *repository, *rootNumber)
	if err != nil {
		fail(err)
	}
	version, err := db.CurrentVersion(ctx, *repository, snapshot.Root.ID)
	if err != nil {
		fail(err)
	}
	if err := syncReviewFeedback(ctx, db, client, *repository, version.ID, *ticketID, *reviewFeedback); err != nil {
		fail(err)
	}
	claim, revisionPrompt, err := acquireTicketClaim(ctx, db, version.ID, *ticketID, config.Runtime.MaxWorkerAttempts, time.Now().UTC())
	if err != nil {
		fail(err)
	}
	if revisionPrompt != "" {
		*prompt = revisionPrompt
	}
	if *prompt == "" {
		fail(fmt.Errorf("run-ticket requires prompt for an active worker run"))
	}
	if *branch == "" {
		*branch = "workflow/ticket-" + fmt.Sprint(claim.TicketNumber)
	}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}, Runtime: worker.DockerRuntime{}, GatewayURL: *gatewayURL}
	candidate, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: *source, Branch: *branch, Prompt: *prompt, Publication: store.CandidatePublication{Repository: *repository, Branch: *branch, ExpectedRemoteHead: *expectedHead, ExpectRemoteAbsent: *expectAbsent, Title: claim.TicketTitle}})
	if err != nil {
		fail(err)
	}
	encoded, _ := json.MarshalIndent(candidate, "", "  ")
	fmt.Println(string(encoded))
}

func syncReviewFeedback(ctx context.Context, db *store.Store, client *github.Client, repository, versionID string, ticketID int64, manual string) error {
	var feedback []store.ReviewFeedback
	delivery, err := db.TicketDelivery(ctx, versionID, ticketID)
	if err == nil {
		terminal, err := (github.DeliveredReconciler{Store: db, Client: client}).ReconcileTicket(ctx, delivery)
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
			feedback = append(feedback, store.ReviewFeedback{Source: event.Source, EventID: event.EventID, Author: event.Author, Body: event.Body})
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

func acquireTicketClaim(ctx context.Context, db *store.Store, versionID string, ticketID int64, maxAttempts int, now time.Time) (store.TicketClaim, string, error) {
	claim, err := db.CurrentClaim(ctx, versionID, ticketID)
	if err == nil {
		return claim, "", nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.TicketClaim{}, "", err
	}
	revision, revisionPrompt, revisionErr := db.ClaimQueuedReviewRevision(ctx, versionID, ticketID, 30*time.Minute, now, 1, maxAttempts)
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
		VersionID:       versionID,
		TicketID:        ticketID,
		Owner:           owner,
		MaxParallelRuns: 1,
		MaxAttempts:     maxAttempts,
		LeaseTTL:        30 * time.Minute,
		Now:             now,
	})
	if replacementErr != nil {
		return store.TicketClaim{}, "", replacementErr
	}
	return replacement, "", nil
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
	var client *github.Client
	_, err = admitGatewayCredential(ctx, db, func(token string) error {
		client = github.NewClient(*githubURL, token, nil).WithRepositoryOwner(config.GitHub.Credential.Owner)
		return requireOwnerGuardedControlPlaneRepository(ctx, client, *repository)
	})
	if err != nil {
		fail(err)
	}
	marked, err := (github.DeliveredReconciler{Store: db, Client: client}).Reconcile(ctx, *repository)
	if err != nil {
		fail(persistGatewayCredentialAdmissionError(ctx, db, err, time.Now().UTC()))
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
	workspaceRetention := flags.Duration("workspace-retention", 7*24*time.Hour, "retention period before closed Ticket Workspaces are reclaimed")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	gatewayControlURLOverride := flags.String("gateway-control-url", "", "optional host-side Gateway URL; defaults to gateway-url")
	gatewayControlToken := flags.String("gateway-control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	once := flags.Bool("once", false, "perform one durable reconciliation pass")
	interval := flags.Duration("interval", time.Minute, "continuous polling interval")
	maxParallelRuns := flags.Int("max-parallel-runs", 1, "maximum concurrent Worker Runs")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *gatewayURL == "" || *gatewayControlToken == "" || *interval <= 0 || *maxParallelRuns <= 0 || *workspaceRetention <= 0 {
		fmt.Fprintln(os.Stderr, "poll-github requires repository, approved plan root, workspace configuration, Gateway URL and control credential, positive interval, and positive parallelism")
		os.Exit(2)
	}
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	var workers sync.WaitGroup
	var workerError error
	var workerErrorMu sync.Mutex
	launch := func(ctx context.Context, claim store.TicketClaim, prompt, branch, expectedHead string, expectAbsent bool) error {
		workerCtx := context.WithoutCancel(ctx)
		run := func() {
			err := runClaimWorker(workerCtx, db, config, *repository, *source, *workspaceRoot, *stateRoot, *gatewayURL, claim, prompt, branch, expectedHead, expectAbsent)
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
		controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}, Runtime: worker.DockerRuntime{}, GatewayURL: *gatewayURL}
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
		poller := github.Poller{Store: db, InboxProjector: projector, MaxFailures: config.Runtime.MaxWorkerAttempts, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts, MaxParallelRuns: *maxParallelRuns}
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
		_, err = admitPollGitHubCredential(ctx, poller, db, *repository, func(token string) error {
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
		result, err := poller.PollWithBootstrap(ctx, *repository, bootstrap, func(ctx context.Context, bootstrapped bool) (github.BootstrapControlResult, error) {
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
			activeRoot, err := db.SchedulerRoot(ctx, *repository, *rootNumber, time.Now().UTC())
			if err != nil {
				return controlResult, err
			}
			workspaceManager := agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}
			dispatcher := scheduler.Dispatcher{Store: db, Reader: client, Projector: projector, MaxParallelRuns: *maxParallelRuns, LeaseTTL: 30 * time.Minute, Recovery: agent.RecoveryInspector{Containers: worker.DockerRuntime{}, Workspace: workspaceManager}, HostPressure: worker.DockerRuntime{}}
			if err := dispatcher.Recover(ctx, *repository, activeRoot); err != nil {
				return controlResult, err
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
				claim, claimErr := dispatcher.Claim(ctx, *repository, activeRoot, 0, "workflow-control-plane")
				if claimErr == nil {
					branch := "workflow/ticket-" + fmt.Sprint(claim.TicketNumber)
					if err := launch(ctx, claim, "Implement ticket #"+fmt.Sprint(claim.TicketNumber)+": "+claim.TicketTitle, branch, "", true); err != nil {
						return controlResult, err
					}
					continue
				}
				if errors.Is(claimErr, store.ErrNoReadyTickets) || errors.Is(claimErr, store.ErrCapacity) || errors.Is(claimErr, store.ErrNotReady) {
					return controlResult, nil
				}
				return controlResult, claimErr
			}
		})
		if shouldLogNeedsAttentionError(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		if err != nil && !errors.Is(err, store.ErrNotReady) && !errors.Is(err, store.ErrNeedsAttention) {
			err = persistGatewayCredentialAdmissionError(ctx, db, err, time.Now().UTC())
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

func runClaimWorker(ctx context.Context, db *store.Store, config doctor.Config, repository, source, workspaceRoot, stateRoot, gatewayURL string, claim store.TicketClaim, prompt, branch, expectedHead string, expectAbsent bool) error {
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: workspaceRoot, CodexStateRoot: stateRoot}, Runtime: worker.DockerRuntime{}, GatewayURL: gatewayURL}
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
	if _, err := db.AnswerWorkflowQuestionAndQueueInboxProjection(ctx, *repository, *questionID, *answer, time.Now().UTC()); err != nil {
		fail(err)
	}
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
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		_ = db.Close()
		fail(err)
	}
	credentialSource := func(ctx context.Context) (string, error) {
		return admitGatewayCredential(ctx, db, nil)
	}
	remote := &github.DeliveryRemote{Client: github.NewClient(*githubURL, "", nil).WithRepositoryOwner(config.GitHub.Credential.Owner), Store: db, PushURL: *pushURL, CredentialSource: credentialSource}
	gateway, err := delivery.NewGateway(db, remote)
	if err != nil {
		_ = db.Close()
		fail(err)
	}
	if _, err := credentialSource(context.Background()); shouldPauseGatewayForCredential(err) {
		if pauseErr := db.PauseGatewayWrites(context.Background(), "Gateway Credential is unavailable; replace and verify it to resume writes", time.Now().UTC()); pauseErr != nil {
			_ = db.Close()
			fail(pauseErr)
		}
	}
	if err := gateway.QueueGatewayCredentialInboxProjections(context.Background()); err != nil {
		_ = db.Close()
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
		_ = db.Close()
		fail(err)
	}
	_ = db.Close()
}

func gatewayCredential(ctx context.Context) (string, error) {
	token, err := credential.NewStore().Get(ctx, credential.GatewayTarget)
	if err != nil {
		return "", fmt.Errorf("load Gateway Credential from Windows Credential Manager: %w", err)
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "github_pat_") {
		return "", fmt.Errorf("%w: Gateway Credential is not a fine-grained PAT", delivery.ErrGatewayCredentialRejected)
	}
	return token, nil
}

func verifiedGatewayCredential(ctx context.Context, database *store.Store) (string, error) {
	token, err := gatewayCredential(ctx)
	if err != nil {
		return "", err
	}
	verification, err := database.GatewayCredentialVerification(ctx)
	if err != nil {
		return "", gatewayCredentialVerificationError(err)
	}
	if credential.Fingerprint(token) != verification.FingerprintSHA256 {
		return "", fmt.Errorf("%w: Gateway Credential differs from the verified credential", delivery.ErrGatewayCredentialRejected)
	}
	return token, nil
}

func gatewayCredentialVerificationError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: Gateway Credential has no persisted verification", delivery.ErrGatewayCredentialRejected)
	}
	return gatewayCredentialVerificationStoreError{err: fmt.Errorf("read Gateway Credential verification: %w", err)}
}

type gatewayCredentialVerificationStoreError struct {
	err error
}

func (e gatewayCredentialVerificationStoreError) Error() string          { return e.err.Error() }
func (e gatewayCredentialVerificationStoreError) Unwrap() error          { return e.err }
func (e gatewayCredentialVerificationStoreError) PollStoreFailure() bool { return true }

func shouldPauseGatewayForCredential(err error) bool {
	return errors.Is(err, credential.ErrNotFound) || errors.Is(err, delivery.ErrGatewayCredentialRejected)
}

func recordPollAdmissionFailure(ctx context.Context, poller github.Poller, repository string, admissionErr error) error {
	if shouldPauseGatewayForCredential(admissionErr) && !errors.Is(admissionErr, delivery.ErrGatewayCredentialRejected) {
		admissionErr = fmt.Errorf("%w: Gateway Credential is unavailable: %w", delivery.ErrGatewayCredentialRejected, admissionErr)
	}
	_, err := poller.RecordAdmissionFailure(ctx, repository, admissionErr)
	return err
}

func admitPollGitHubCredential(ctx context.Context, poller github.Poller, database *store.Store, repository string, authenticate func(string) error) (string, error) {
	if err := poller.Ready(ctx, repository); err != nil {
		return "", err
	}
	token, err := verifiedGatewayCredential(ctx, database)
	if err == nil && authenticate != nil {
		err = authenticate(token)
	}
	if err != nil {
		var authenticationFailure interface{ AuthenticationFailure() bool }
		if errors.As(err, &authenticationFailure) && authenticationFailure.AuthenticationFailure() {
			err = fmt.Errorf("%w: GitHub rejected Gateway Credential: %w", delivery.ErrGatewayCredentialRejected, err)
		} else if errors.Is(err, credential.ErrNotFound) {
			err = fmt.Errorf("%w: Gateway Credential is missing: %w", delivery.ErrGatewayCredentialRejected, err)
		}
		return "", err
	}
	return token, nil
}

func persistGatewayCredentialPause(ctx context.Context, database *store.Store, credentialErr error, now time.Time) error {
	if !shouldPauseGatewayForCredential(credentialErr) {
		return credentialErr
	}
	if err := database.PauseGatewayWrites(ctx, "Gateway Credential is unavailable; replace and verify it to resume writes", now); err != nil {
		return errors.Join(credentialErr, fmt.Errorf("persist Gateway Credential pause: %w", err))
	}
	return credentialErr
}

func admitGatewayCredential(ctx context.Context, database *store.Store, authenticate func(string) error) (string, error) {
	token, err := verifiedGatewayCredential(ctx, database)
	if err == nil && authenticate != nil {
		err = authenticate(token)
	}
	if err != nil {
		return "", persistGatewayCredentialAdmissionError(ctx, database, err, time.Now().UTC())
	}
	return token, nil
}

func persistGatewayCredentialAdmissionError(ctx context.Context, database *store.Store, admissionErr error, now time.Time) error {
	var authenticationFailure interface{ AuthenticationFailure() bool }
	if errors.As(admissionErr, &authenticationFailure) && authenticationFailure.AuthenticationFailure() {
		admissionErr = fmt.Errorf("%w: GitHub rejected Gateway Credential: %w", delivery.ErrGatewayCredentialRejected, admissionErr)
	}
	return persistGatewayCredentialPause(ctx, database, admissionErr, now)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
