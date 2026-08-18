package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func runtimeStatusCommand(args []string, output io.Writer) error {
	layout, err := runtimeLayout(args, "status")
	if err != nil {
		return err
	}
	record, err := controlplane.ReadRuntimeRecord(layout)
	var observation controlplane.Observation
	switch {
	case errors.Is(err, os.ErrNotExist):
		observation = (controlplane.Inspector{}).Inspect(context.Background(), nil)
	case err != nil:
		observation = controlplane.Observation{State: controlplane.StateMismatched, Diagnostic: err.Error()}
	default:
		observation = (controlplane.Inspector{}).Inspect(context.Background(), &record)
	}
	return json.NewEncoder(output).Encode(observation)
}

func runtimeStopCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	timeout := flags.Duration("timeout", 10*time.Second, "graceful shutdown timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return errors.New("workflow stop requires flags only and a positive timeout")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	record, err := controlplane.ReadRuntimeRecord(layout)
	if errors.Is(err, os.ErrNotExist) {
		return json.NewEncoder(output).Encode(map[string]string{"status": controlplane.StateStopped})
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := controlplane.Stop(ctx, record, controlplane.Inspector{}); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]string{"status": controlplane.StateStopped})
}

func runtimeLogsCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	lines := flags.Int("lines", 200, "number of trailing lines per log")
	follow := flags.Bool("follow", false, "continue streaming appended log lines")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *lines < 0 {
		return errors.New("workflow logs requires flags only and a non-negative line count")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	stdout, stderr := controlplane.LogPaths(layout)
	for _, item := range []struct{ name, path string }{{"stdout", stdout}, {"stderr", stderr}} {
		if _, err := fmt.Fprintf(output, "==> %s (%s) <==\n", item.name, item.path); err != nil {
			return err
		}
		if err := writeTail(output, item.path, *lines); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !*follow {
		return nil
	}
	return followLogs(context.Background(), output, []string{stdout, stderr})
}

func runtimeConfigureCommand(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("runtime-configure", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	repository := flags.String("repository", "", "canonical owner/repository")
	root := flags.Int64("root", 0, "approved Plan Root issue number")
	source := flags.String("source", "", "absolute local repository path")
	defaultBranch := flags.String("default-branch", "", "canonical default branch")
	maxParallel := flags.Int("max-parallel-runs", 0, "optional maximum parallel Worker Runs")
	codexAuthFile := flags.String("codex-auth-file", "", "absolute Codex authentication source supplied by the invoking integration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*repository == "" && *source == "") || *root <= 0 {
		return errors.New("runtime-configure requires a repository identity or source path and a positive Plan Root issue number")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	ctx := context.Background()
	active, err := launcher.ReadActive(layout.Root)
	if err != nil || active.Readiness != "ready" {
		return errors.New("runtime-configure requires a ready active generation")
	}
	database, err := store.OpenActivated(ctx, filepath.Join(layout.Root, "platform", "generations", active.Generation, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	var config store.RepositoryRuntimeConfiguration
	if *repository != "" {
		config, err = database.RepositoryRuntimeConfiguration(ctx, *repository)
	} else {
		absoluteSource, pathErr := filepath.Abs(*source)
		if pathErr != nil {
			return pathErr
		}
		configs, listErr := database.RepositoryRuntimeConfigurations(ctx)
		if listErr != nil {
			return listErr
		}
		for _, candidate := range configs {
			if strings.EqualFold(filepath.Clean(candidate.SourcePath), filepath.Clean(absoluteSource)) {
				if config.Repository != "" {
					return errors.New("source path matches multiple Repository Runtime Configurations")
				}
				config = candidate
			}
		}
		if config.Repository == "" {
			err = store.ErrNotFound
		}
	}
	if err != nil {
		return fmt.Errorf("read admitted repository runtime configuration: %w", err)
	}
	config.RootIssueNumber = *root
	if *source != "" {
		config.SourcePath = *source
	}
	if *defaultBranch != "" {
		config.DefaultBranch = *defaultBranch
	}
	if *maxParallel > 0 {
		config.MaxParallelRuns = *maxParallel
	}
	if *codexAuthFile != "" {
		if !filepath.IsAbs(*codexAuthFile) {
			return errors.New("explicit Codex authentication source must be absolute")
		}
		absoluteAuth, authErr := filepath.Abs(*codexAuthFile)
		if authErr != nil {
			return fmt.Errorf("resolve explicit Codex authentication source: %w", authErr)
		}
		config.CodexAuthFile = filepath.Clean(absoluteAuth)
		if authErr := codexauth.ValidateChatGPT(config.CodexAuthFile); authErr != nil {
			return fmt.Errorf("validate explicit Codex authentication source: %w", authErr)
		}
	}
	repositoryKey := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.ToLower(config.Repository))
	if config.WorkspaceRoot == "" {
		config.WorkspaceRoot = filepath.Join(layout.Workspaces, repositoryKey)
	}
	if config.StateRoot == "" {
		config.StateRoot = filepath.Join(layout.State, "codex", repositoryKey)
	}
	if config.CodexAuthFile == "" {
		config.CodexAuthFile, err = codexauth.ResolveChatGPT(ctx)
		if err != nil {
			return fmt.Errorf("resolve invoking Codex login: %w", err)
		}
	}
	config.UpdatedAt = time.Now().UTC()
	if err := config.Ready(); err != nil {
		return fmt.Errorf("complete repository runtime configuration: %w", err)
	}
	if err := database.RecordRepositoryRuntimeConfiguration(ctx, config); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"status": "configured", "repository": config.Repository, "root_issue_number": config.RootIssueNumber})
}

func runtimeLayout(args []string, name string) (workflowhome.Layout, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	if err := flags.Parse(args); err != nil {
		return workflowhome.Layout{}, err
	}
	if flags.NArg() != 0 {
		return workflowhome.Layout{}, fmt.Errorf("workflow %s accepts flags only", name)
	}
	return workflowhome.Resolve(*homeOverride)
}

func writeTail(output io.Writer, path string, count int) error {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		if count == 0 {
			continue
		}
		if len(lines) == count {
			copy(lines, lines[1:])
			lines[len(lines)-1] = scanner.Text()
		} else {
			lines = append(lines, scanner.Text())
		}
	}
	for _, line := range lines {
		if _, err := io.WriteString(output, line+"\n"); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func followLogs(ctx context.Context, output io.Writer, paths []string) error {
	offsets := map[string]int64{}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			offsets[path] = info.Size()
		}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, path := range paths {
				file, err := os.Open(path)
				if err != nil {
					continue
				}
				_, _ = file.Seek(offsets[path], io.SeekStart)
				written, copyErr := io.Copy(output, file)
				file.Close()
				offsets[path] += written
				if copyErr != nil && !strings.Contains(copyErr.Error(), "closed") {
					return copyErr
				}
			}
		}
	}
}
