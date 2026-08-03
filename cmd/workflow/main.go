package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/agent"
	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: workflow <doctor|run-ticket|reconcile-delivered>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "doctor":
		runDoctor(os.Args[2:])
	case "run-ticket":
		runTicket(os.Args[2:])
	case "reconcile-delivered":
		runReconcileDelivered(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: workflow <doctor|run-ticket|reconcile-delivered>")
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
	branch := flags.String("branch", "", "ticket branch")
	token := flags.String("github-token", os.Getenv("WORKFLOW_GITHUB_TOKEN"), "Gateway GitHub credential")
	expectedHead := flags.String("expected-remote-head", "", "current remote ticket branch head")
	expectAbsent := flags.Bool("expect-remote-absent", true, "require the ticket branch to be absent")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	_ = flags.Parse(args)
	if *repository == "" || *rootNumber <= 0 || *ticketID == 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *prompt == "" || *token == "" || (*expectedHead != "") == *expectAbsent {
		fmt.Fprintln(os.Stderr, "run-ticket requires repository, root, ticket-id, source, workspace-root, state-root, prompt, credential, and exactly one remote-head expectation")
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
	claim, err := db.CurrentClaim(ctx, version.ID, *ticketID)
	if err != nil {
		fail(err)
	}
	if *branch == "" {
		*branch = "workflow/ticket-" + fmt.Sprint(claim.TicketNumber)
	}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: *workspaceRoot, CodexStateRoot: *stateRoot}, Runtime: worker.DockerRuntime{}, ImageDigest: config.Worker.Image, ToolVersions: map[string]string{"codex": config.Codex.Version}}
	candidate, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: *source, Branch: *branch, Prompt: *prompt})
	if err != nil {
		fail(err)
	}
	session, err := db.TicketSession(ctx, version.ID, *ticketID)
	if err != nil {
		fail(err)
	}
	remote := github.DeliveryRemote{Client: client, Pusher: github.WorkspacePusher{WorkspacePath: session.WorkspacePath, Token: *token}}
	gateway := delivery.Gateway{Store: db, Remote: remote}
	baseRequest := store.DeliveryRequest{RunID: claim.RunID, LeaseToken: claim.LeaseToken, LeaseGeneration: claim.LeaseGeneration, Repository: *repository, Branch: *branch, CommitSHA: candidate.Commit}
	push := baseRequest
	push.Operation, push.ExpectedRemoteHead, push.ExpectRemoteAbsent = store.DeliveryPushCandidate, *expectedHead, *expectAbsent
	dispatchDelivery(ctx, gateway, push)
	pr := baseRequest
	pr.Operation, pr.ExpectedRemoteHead, pr.Title, pr.Body = store.DeliveryUpsertPR, candidate.Commit, claim.TicketTitle, candidateSummary(candidate.StructuredOutput)
	dispatchDelivery(ctx, gateway, pr)
	encoded, _ := json.MarshalIndent(candidate, "", "  ")
	fmt.Println(string(encoded))
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

func dispatchDelivery(ctx context.Context, gateway delivery.Gateway, request store.DeliveryRequest) {
	outbox, err := gateway.Submit(ctx, request)
	if err != nil {
		fail(err)
	}
	if err := gateway.Dispatch(ctx, outbox.IdempotencyKey); err != nil {
		fail(err)
	}
}

func candidateSummary(output []byte) string {
	var result struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(output, &result); err != nil || strings.TrimSpace(result.Summary) == "" {
		return "Worker completed candidate delivery."
	}
	return result.Summary
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
