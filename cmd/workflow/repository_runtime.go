package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type commandRepositoryRunner struct {
	Executable string
	Layout     workflowhome.Layout
	Owner      string
}

func (r commandRepositoryRunner) Run(ctx context.Context, config store.RepositoryRuntimeConfiguration) error {
	if err := config.Ready(); err != nil {
		return err
	}
	if r.Executable == "" || r.Owner == "" {
		return errors.New("repository process runner is incomplete")
	}
	port, err := availableGatewayPort()
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	controlToken := hex.EncodeToString(secret)
	gatewayArgs, pollArgs := r.processArguments(config, port, controlToken)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	gateway := r.command(runCtx, gatewayArgs...)
	if err := gateway.Start(); err != nil {
		return fmt.Errorf("start repository Gateway: %w", err)
	}
	gatewayDone := make(chan error, 1)
	go func() { gatewayDone <- gateway.Wait() }()
	if err := waitForGateway(runCtx, "127.0.0.1:"+strconv.Itoa(port), gatewayDone); err != nil {
		return err
	}
	poll := r.command(runCtx, pollArgs...)
	if err := poll.Start(); err != nil {
		cancel()
		<-gatewayDone
		return fmt.Errorf("start repository business loop: %w", err)
	}
	pollDone := make(chan error, 1)
	go func() { pollDone <- poll.Wait() }()
	select {
	case <-ctx.Done():
		cancel()
		<-gatewayDone
		<-pollDone
		return nil
	case err := <-gatewayDone:
		cancel()
		<-pollDone
		return fmt.Errorf("repository Gateway stopped: %w", err)
	case err := <-pollDone:
		cancel()
		<-gatewayDone
		return fmt.Errorf("repository business loop stopped: %w", err)
	}
}

func (r commandRepositoryRunner) processArguments(config store.RepositoryRuntimeConfiguration, port int, controlToken string) ([]string, []string) {
	databasePath := filepath.Join(r.Layout.State, "workflow.db")
	listen := "0.0.0.0:" + strconv.Itoa(port)
	controlURL := "http://127.0.0.1:" + strconv.Itoa(port)
	workerURL := "http://host.docker.internal:" + strconv.Itoa(port)
	credentialPath := `state\credentials\github.pat`
	gateway := []string{"gateway", "--database", databasePath, "--listen", listen, "--control-token", controlToken, "--github-url", config.GitHubAPIURL, "--owner", r.Owner, "--credential-relative-path", credentialPath}
	poll := []string{"poll-github", "--database", databasePath, "--repository", config.Repository, "--root", strconv.FormatInt(config.RootIssueNumber, 10), "--github-url", config.GitHubAPIURL, "--source", config.SourcePath, "--workspace-root", config.WorkspaceRoot, "--state-root", config.StateRoot, "--codex-auth-file", config.CodexAuthFile, "--workspace-retention", config.WorkspaceRetention.String(), "--gateway-url", workerURL, "--gateway-control-url", controlURL, "--gateway-control-token", controlToken, "--interval", config.PollInterval.String(), "--max-parallel-runs", strconv.Itoa(config.MaxParallelRuns), "--owner", r.Owner, "--credential-relative-path", credentialPath}
	return gateway, poll
}

func (r commandRepositoryRunner) command(ctx context.Context, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, r.Executable, args...)
	command.Env = append(os.Environ(), "WORKFLOW_HOME="+r.Layout.Root)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command
}

func availableGatewayPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForGateway(ctx context.Context, address string, done <-chan error) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			return fmt.Errorf("repository Gateway stopped during startup: %w", err)
		case <-deadline.C:
			return errors.New("repository Gateway readiness timed out")
		case <-ticker.C:
			connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				connection.Close()
				return nil
			}
		}
	}
}
