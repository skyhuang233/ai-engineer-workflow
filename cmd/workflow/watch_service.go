package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/skyhuang233/workflow/internal/codetaskintake"
	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

// watchServiceCommand is the foreground process owned by Task Scheduler or
// launchd. It contains no PID/runtime record: the host lifecycle manager owns
// restarts and the Watch success checkpoint proves useful operation.
func watchServiceCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("watch-service", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	home := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	databasePath := flags.String("database", "", "advanced absolute SQLite Watch Store override")
	interval := flags.Duration("interval", time.Minute, "Issue observation interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("workflow watch-service accepts flags only")
	}
	layout, err := workflowhome.Resolve(*home)
	if err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	if *databasePath == "" {
		*databasePath = filepath.Join(layout.State, "workflow.db")
	}
	if !filepath.IsAbs(*databasePath) {
		return errors.New("--database override must be absolute")
	}
	database, err := store.Open(context.Background(), *databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report := func(repository string, err error) {
		fmt.Fprintf(os.Stderr, "repository watch %s: %v\n", repository, err)
	}
	return (controlplane.RepositoryWatchSupervisor{Store: database, Observer: controlplane.GitHubCLIIssueObserver{}, Intake: codetaskintake.StoreIntake{Store: database}, Interval: *interval, Report: report}).Run(ctx)
}
