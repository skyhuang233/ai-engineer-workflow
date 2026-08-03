package main

import (
	"context"
	"crypto/sha256"
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
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/scheduler"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: workflow <doctor|run-ticket|gateway|poll-github|reconcile-delivered|answer-inbox>")
		os.Exit(2)
	}
	switch os.Args[1] {
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
	default:
		fmt.Fprintln(os.Stderr, "usage: workflow <doctor|run-ticket|gateway|poll-github|reconcile-delivered|answer-inbox>")
		os.Exit(2)
	}
}

func runDoctor(args []string) {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", filepath.Join(os.TempDir(), "workflow-doctor.db"), "SQLite probe database")
	reportPath := flags.String("report", "", "optional Markdown report path")
	_ = flags.Parse(args)

	config, err := doctor.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
				ExactCommit:  config.NoMistakes.UpstreamCommit[:7],
			},
		},
		doctor.CodexResumeCheck{Executor: doctor.OSExecutor{}},
		doctor.SQLiteCheck{Path: *databasePath},
		doctor.DockerCheck{Worker: config.Worker},
		doctor.WorkerRegistryCheck{Image: config.Worker.Image},
		doctor.GitHubCredentialCheck{Pin: config.GitHub.Credential},
		doctor.GitHubCheck{GitHub: config.GitHub, NoMistakes: config.NoMistakes},
	}}
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
	if !report.Passed() {
		os.Exit(1)
	}
}

func runTicket(args []string) {
	flags := flag.NewFlagSet("run-ticket", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", "workflow.db", "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	rootNumber := flags.Int64("root", 0, "plan root issue number")
	ticketID := flags.Int64("ticket-id", 0, "GitHub ticket node ID")
	source := flags.String("source", "", "absolute local repository path")
	workspaceRoot := flags.String("workspace-root", "", "absolute Ticket Workspace root")
	stateRoot := flags.String("state-root", "", "absolute Codex state root")
	prompt := flags.String("prompt", "", "Worker prompt")
	reviewFeedback := flags.String("review-feedback", "", "human pull-request feedback to queue for the next revision round")
	branch := flags.String("branch", "", "ticket branch")
	token := flags.String("github-token", os.Getenv("WORKFLOW_GITHUB_TOKEN"), "GitHub read credential")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	expectedHead := flags.String("expected-remote-head", "", "current remote ticket branch head")
	expectAbsent := flags.Bool("expect-remote-absent", true, "require the ticket branch to be absent")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *ticketID == 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *token == "" || *gatewayURL == "" || (*expectedHead != "") == *expectAbsent {
		fmt.Fprintln(os.Stderr, "run-ticket requires repository, root, ticket-id, source, workspace-root, state-root, read credential, Gateway URL, and exactly one remote-head expectation")
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
	client := github.NewClient(*githubURL, *token, nil)
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
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}, Runtime: worker.DockerRuntime{}, ImageDigest: config.Worker.Image, ToolVersions: map[string]string{"no-mistakes": config.NoMistakes.Version, "codex": config.Codex.Version}, GatewayURL: *gatewayURL}
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
	databasePath := flags.String("database", "workflow.db", "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	token := flags.String("github-token", os.Getenv("WORKFLOW_GITHUB_TOKEN"), "Gateway GitHub credential")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "reconcile-delivered requires repository and credential")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	marked, err := (github.DeliveredReconciler{Store: db, Client: github.NewClient(*githubURL, *token, nil)}).Reconcile(ctx, *repository)
	if err != nil {
		fail(err)
	}
	fmt.Println(marked)
}

func runPollGitHub(args []string) {
	flags := flag.NewFlagSet("poll-github", flag.ExitOnError)
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", "workflow.db", "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	rootNumber := flags.Int64("root", 0, "approved plan root issue number")
	token := flags.String("github-token", os.Getenv("WORKFLOW_GITHUB_TOKEN"), "GitHub read credential")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	source := flags.String("source", "", "absolute local repository path for review revisions")
	workspaceRoot := flags.String("workspace-root", "", "absolute Ticket Workspace root")
	stateRoot := flags.String("state-root", "", "absolute Codex state root")
	workspaceRetention := flags.Duration("workspace-retention", 7*24*time.Hour, "retention period before closed Ticket Workspaces are reclaimed")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	gatewayControlToken := flags.String("gateway-control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	once := flags.Bool("once", false, "perform one durable reconciliation pass")
	interval := flags.Duration("interval", time.Minute, "continuous polling interval")
	maxParallelRuns := flags.Int("max-parallel-runs", 1, "maximum concurrent Worker Runs")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *token == "" || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *gatewayURL == "" || *gatewayControlToken == "" || *interval <= 0 || *maxParallelRuns <= 0 || *workspaceRetention <= 0 {
		fmt.Fprintln(os.Stderr, "poll-github requires repository, approved plan root, read credential, workspace configuration, Gateway URL and control credential, positive interval, and positive parallelism")
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
		controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}, Runtime: worker.DockerRuntime{}, ImageDigest: config.Worker.Image, ToolVersions: map[string]string{"no-mistakes": config.NoMistakes.Version, "codex": config.Codex.Version}, GatewayURL: *gatewayURL}
		return controller.RetryDelivery(ctx, claim)
	}
	poller := github.Poller{Store: db, Client: github.NewClient(*githubURL, *token, nil), LaunchReview: launcher, InboxProjector: delivery.HTTPProjector{URL: *gatewayURL, ControlPlaneToken: *gatewayControlToken}, MaxFailures: config.Runtime.MaxWorkerAttempts, MaxWorkerAttempts: config.Runtime.MaxWorkerAttempts, MaxParallelRuns: *maxParallelRuns}
	var lastPollResult github.PollResult
	poll := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		client := github.NewClient(*githubURL, *token, nil)
		projector := delivery.HTTPProjector{URL: *gatewayURL, ControlPlaneToken: *gatewayControlToken}
		result, err := poller.PollWith(ctx, *repository, func(ctx context.Context) error {
			activeRoot, err := db.SchedulerRoot(ctx, *repository, *rootNumber, time.Now().UTC())
			if err != nil {
				return err
			}
			activator := plan.Activator{Reader: client, Projector: projector, Store: db}
			if _, err := activator.Activate(ctx, *repository, activeRoot); err != nil {
				return err
			}
			activeRoot, err = db.SchedulerRoot(ctx, *repository, *rootNumber, time.Now().UTC())
			if err != nil {
				return err
			}
			workspaceManager := agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}
			dispatcher := scheduler.Dispatcher{Store: db, Reader: client, Projector: projector, MaxParallelRuns: *maxParallelRuns, LeaseTTL: 30 * time.Minute, Recovery: agent.RecoveryInspector{Containers: worker.DockerRuntime{}, Workspace: workspaceManager}, HostPressure: worker.DockerRuntime{}}
			if err := dispatcher.Recover(ctx, *repository, activeRoot); err != nil {
				return err
			}
			if _, err := workspaceManager.ReclaimClosed(ctx, db, *workspaceRetention, time.Now().UTC()); err != nil {
				return err
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
				return err
			}
			for {
				claim, claimErr := dispatcher.Claim(ctx, *repository, activeRoot, 0, "workflow-control-plane")
				if claimErr == nil {
					branch := "workflow/ticket-" + fmt.Sprint(claim.TicketNumber)
					if err := launch(ctx, claim, "Implement ticket #"+fmt.Sprint(claim.TicketNumber)+": "+claim.TicketTitle, branch, "", true); err != nil {
						return err
					}
					continue
				}
				if errors.Is(claimErr, store.ErrNoReadyTickets) || errors.Is(claimErr, store.ErrCapacity) || errors.Is(claimErr, store.ErrNotReady) {
					return nil
				}
				return claimErr
			}
		})
		if err != nil && !errors.Is(err, store.ErrNotReady) && !errors.Is(err, store.ErrNeedsAttention) {
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
		_ = poll()
		time.Sleep(nextPollDelay(db, *repository, *interval, lastPollResult, time.Now().UTC()))
	}
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
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: workspaceRoot, CodexStateRoot: stateRoot}, Runtime: worker.DockerRuntime{}, ImageDigest: config.Worker.Image, ToolVersions: map[string]string{"no-mistakes": config.NoMistakes.Version, "codex": config.Codex.Version}, GatewayURL: gatewayURL}
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
	databasePath := flags.String("database", "workflow.db", "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	questionID := flags.String("question", "", "stable Workflow Inbox question ID")
	answer := flags.String("answer", "", "human decision")
	gatewayURL := flags.String("gateway-url", "", "credential-isolated GitHub Write Gateway URL")
	gatewayControlToken := flags.String("gateway-control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	_ = flags.Parse(args)
	if *repository == "" || *questionID == "" || *answer == "" || *gatewayURL == "" || *gatewayControlToken == "" {
		fmt.Fprintln(os.Stderr, "answer-inbox requires repository, question, answer, Gateway URL, and control credential")
		os.Exit(2)
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.AnswerWorkflowQuestion(ctx, *repository, *questionID, *answer, time.Now().UTC()); err != nil {
		fail(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, *repository, 0)
	if err != nil {
		fail(err)
	}
	projected := make([]plan.WorkflowQuestion, 0, len(questions))
	for _, open := range questions {
		projected = append(projected, plan.WorkflowQuestion{ID: open.ID, Prompt: open.Prompt, Repository: open.Repository, PlanNumber: open.RootNumber, TicketNumber: open.TicketNumber, PullRequest: open.PullRequest, Commit: open.Commit, Finding: open.Kind, Diagnostics: open.Diagnostics, Evidence: open.Evidence})
	}
	if err := (delivery.HTTPProjector{URL: *gatewayURL, ControlPlaneToken: *gatewayControlToken}).ProjectWorkflowInbox(ctx, *repository, projected); err != nil {
		fail(err)
	}
}

func runGateway(args []string) {
	flags := flag.NewFlagSet("gateway", flag.ExitOnError)
	databasePath := flags.String("database", "workflow.db", "SQLite control-plane database")
	listen := flags.String("listen", "", "Gateway listen address")
	token := flags.String("github-token", os.Getenv("WORKFLOW_GITHUB_TOKEN"), "Gateway GitHub credential")
	controlToken := flags.String("control-token", os.Getenv("WORKFLOW_GATEWAY_CONTROL_TOKEN"), "Gateway control-plane credential")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	pushURL := flags.String("push-url", "", "optional HTTPS Git push URL")
	gitBinary := flags.String("git", "git", "Git executable")
	outboxInterval := flags.Duration("outbox-interval", time.Second, "durable outbox recovery interval")
	_ = flags.Parse(args)
	if *listen == "" || *token == "" || *controlToken == "" || *outboxInterval <= 0 {
		fmt.Fprintln(os.Stderr, "gateway requires listen address, GitHub credential, and control-plane credential")
		os.Exit(2)
	}
	db, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		fail(err)
	}
	remote := github.DeliveryRemote{Client: github.NewClient(*githubURL, *token, nil), Store: db, Token: *token, PushURL: *pushURL, GitBinary: *gitBinary}
	gateway := delivery.Gateway{Store: db, Remote: remote}
	go func() {
		for {
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

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
