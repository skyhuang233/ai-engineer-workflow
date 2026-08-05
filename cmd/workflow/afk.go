package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/skyhuang233/workflow/internal/delivery"
	"github.com/skyhuang233/workflow/internal/doctor"
	"github.com/skyhuang233/workflow/internal/github"
	"github.com/skyhuang233/workflow/internal/store"
)

type embeddedGateway struct {
	cancel       context.CancelFunc
	server       *http.Server
	database     *store.Store
	serveDone    chan error
	recoveryDone chan struct{}
	hostURL      string
	workerURL    string
}

func runAFK(args []string) {
	if err := executeAFK(args); err != nil {
		fail(err)
	}
}

func executeAFK(args []string) error {
	return executeAFKWithDependencies(args, afkDependencies{})
}

type afkDependencies struct {
	AdmitCredential gatewayCredentialAdmitter
	GatewayRemote   delivery.Remote
	Runtime         controlPlaneRuntime
}

func executeAFKWithDependencies(args []string, dependencies afkDependencies) error {
	flags := flag.NewFlagSet("afk", flag.ContinueOnError)
	iterations := flags.Int("iterations", 100, "number of bounded reconciliation passes")
	configPath := flags.String("config", "config/toolchain.json", "toolchain baseline")
	databasePath := flags.String("database", defaultControlPlaneDatabase, "SQLite control-plane database")
	repository := flags.String("repository", "", "GitHub owner/repository")
	rootNumber := flags.Int64("root", 0, "approved plan root issue number")
	githubURL := flags.String("github-url", "https://api.github.com", "GitHub API base URL")
	pushURL := flags.String("push-url", "", "optional HTTPS Git push URL")
	source := flags.String("source", "", "absolute accepted repository path")
	workspaceRoot := flags.String("workspace-root", "", "absolute Ticket Workspace root")
	stateRoot := flags.String("state-root", "", "absolute Codex state root")
	workspaceRetention := flags.Duration("workspace-retention", 7*24*time.Hour, "retention period before closed Ticket Workspaces are reclaimed")
	pollInterval := flags.Duration("poll-interval", time.Minute, "delay between bounded reconciliation passes")
	maxParallelRuns := flags.Int("max-parallel-runs", 1, "maximum concurrent Worker Runs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *iterations <= 0 || *repository == "" || *rootNumber <= 0 || *source == "" || *workspaceRoot == "" || *stateRoot == "" || *workspaceRetention <= 0 || *pollInterval <= 0 || *maxParallelRuns <= 0 {
		return errors.New("afk requires positive iterations, repository, approved plan root, workspace configuration, retention, and parallelism")
	}
	admitCredential := dependencies.AdmitCredential
	if admitCredential == nil {
		admitCredential = admitGatewayCredential
	}

	token, err := newControlPlaneToken()
	if err != nil {
		return err
	}
	gateway, err := startEmbeddedGateway(context.Background(), embeddedGatewayOptions{
		ConfigPath:       *configPath,
		DatabasePath:     *databasePath,
		Listen:           "0.0.0.0:0",
		ControlToken:     token,
		GitHubURL:        *githubURL,
		PushURL:          *pushURL,
		RecoveryInterval: time.Second,
		AdmitCredential:  admitCredential,
		Remote:           dependencies.GatewayRemote,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Gateway host endpoint: %s\n", gateway.hostURL)
	fmt.Printf("Gateway Worker endpoint: %s\n", gateway.workerURL)
	for iteration := 1; iteration <= *iterations; iteration++ {
		fmt.Printf("\n===== Codex AFK control-plane pass %d / %d =====\n\n", iteration, *iterations)
		err := executePollGitHubWithAdapters([]string{
			"--once",
			"--repository", *repository,
			"--root", strconv.FormatInt(*rootNumber, 10),
			"--source", *source,
			"--workspace-root", *workspaceRoot,
			"--state-root", *stateRoot,
			"--gateway-url", gateway.hostURL,
			"--worker-gateway-url", gateway.workerURL,
			"--gateway-control-token", token,
			"--max-parallel-runs", strconv.Itoa(*maxParallelRuns),
			"--workspace-retention", workspaceRetention.String(),
			"--config", *configPath,
			"--database", *databasePath,
			"--github-url", *githubURL,
		}, pollGitHubAdapters{AdmitCredential: admitCredential, Runtime: dependencies.Runtime})
		if err != nil {
			return errors.Join(fmt.Errorf("control-plane pass %d: %w", iteration, err), gateway.Close())
		}
		if iteration < *iterations {
			time.Sleep(*pollInterval)
		}
	}
	return gateway.Close()
}

type embeddedGatewayOptions struct {
	ConfigPath       string
	DatabasePath     string
	Listen           string
	ControlToken     string
	GitHubURL        string
	PushURL          string
	RecoveryInterval time.Duration
	AdmitCredential  gatewayCredentialAdmitter
	Remote           delivery.Remote
}

func startEmbeddedGateway(parent context.Context, options embeddedGatewayOptions) (*embeddedGateway, error) {
	if options.Listen == "" || options.ControlToken == "" || options.RecoveryInterval <= 0 {
		return nil, errors.New("embedded Gateway requires listen address, control credential, and recovery interval")
	}
	config, err := doctor.LoadConfig(options.ConfigPath)
	if err != nil {
		return nil, err
	}
	database, err := store.Open(parent, options.DatabasePath)
	if err != nil {
		return nil, err
	}
	admitCredential := options.AdmitCredential
	if admitCredential == nil {
		admitCredential = admitGatewayCredential
	}
	credentialSource := func(ctx context.Context) (string, error) {
		return admitCredential(ctx, database, nil)
	}
	remote := options.Remote
	if remote == nil {
		remote = &github.DeliveryRemote{
			Client:           github.NewClient(options.GitHubURL, "", nil).WithRepositoryOwner(config.GitHub.Credential.Owner),
			Store:            database,
			PushURL:          options.PushURL,
			CredentialSource: credentialSource,
		}
	}
	gateway, err := delivery.NewGateway(database, remote)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	if _, err := credentialSource(parent); shouldPauseGatewayForCredential(err) {
		if pauseErr := database.PauseGatewayWrites(parent, "Gateway Credential is unavailable; replace and verify it to resume writes", time.Now().UTC()); pauseErr != nil {
			_ = database.Close()
			return nil, pauseErr
		}
	}
	if err := gateway.QueueGatewayCredentialInboxProjections(parent); err != nil {
		_ = database.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	hostURL, workerURL, err := gatewayEndpoints(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		_ = database.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	runtime := &embeddedGateway{
		cancel: cancel, server: &http.Server{Handler: delivery.HTTPHandler(gateway, delivery.HTTPOptions{ControlPlaneToken: options.ControlToken})},
		database: database, serveDone: make(chan error, 1), recoveryDone: make(chan struct{}), hostURL: hostURL, workerURL: workerURL,
	}
	go func() {
		err := runtime.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		runtime.serveDone <- err
	}()
	go func() {
		defer close(runtime.recoveryDone)
		ticker := time.NewTicker(options.RecoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := gateway.QueueGatewayCredentialInboxProjections(ctx); err != nil && !errors.Is(err, context.Canceled) {
					fmt.Fprintln(os.Stderr, "gateway credential recovery projection:", err)
				}
				if err := gateway.DispatchPending(ctx, 32); err != nil && !errors.Is(err, context.Canceled) {
					fmt.Fprintln(os.Stderr, "gateway outbox recovery:", err)
				}
			}
		}
	}()
	if err := probeEmbeddedGateway(parent, hostURL, options.ControlToken); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return runtime, nil
}

func (g *embeddedGateway) Close() error {
	g.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := g.server.Shutdown(ctx)
	<-g.recoveryDone
	serveErr := <-g.serveDone
	return errors.Join(shutdownErr, serveErr, g.database.Close())
}

func gatewayEndpoints(address string) (string, string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("parse Gateway listener address: %w", err)
	}
	return "http://127.0.0.1:" + port, "http://host.docker.internal:" + port, nil
}

func newControlPlaneToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate Gateway control token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func probeEmbeddedGateway(ctx context.Context, gatewayURL, token string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/healthz", nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Workflow-Control-Token", token)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("probe embedded Gateway: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("probe embedded Gateway: unexpected status %s", response.Status)
	}
	return nil
}
