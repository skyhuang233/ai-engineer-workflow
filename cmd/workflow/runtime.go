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

	"github.com/skyhuang233/workflow/internal/controlplane"
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
