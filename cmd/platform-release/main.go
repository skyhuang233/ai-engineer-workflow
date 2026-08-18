package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/platformrelease"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("platform-release", flag.ContinueOnError)
	executable := flags.String("workflow-exe", "", "Windows amd64 workflow.exe")
	payload := flags.String("payload", "", "staged package payload root")
	output := flags.String("output", "", "release output directory")
	version := flags.String("version", "", "platform version")
	sourceCommit := flags.String("source-commit", "", "accepted source commit")
	runID := flags.Int64("github-actions-run-id", 0, "publisher Actions run ID")
	dockerVersion := flags.String("docker-version", "", "Docker Desktop version")
	dockerURL := flags.String("docker-installer-url", "", "Docker Desktop Windows amd64 installer URL")
	dockerSHA := flags.String("docker-installer-sha256", "", "Docker Desktop Windows amd64 installer SHA-256")
	workerImage := flags.String("worker-image", "", "immutable Worker image reference")
	setupExecutable := flags.String("setup-exe", "", "Windows amd64 workflow-setup.exe")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("platform-release does not accept positional arguments")
	}
	if err := platformrelease.ValidatePlatformVersion(*version); err != nil {
		return fmt.Errorf("-version: %w", err)
	}
	for name, value := range map[string]string{"workflow-exe": *executable, "setup-exe": *setupExecutable, "payload": *payload, "output": *output, "version": *version, "source-commit": *sourceCommit, "docker-version": *dockerVersion, "docker-installer-url": *dockerURL, "docker-installer-sha256": *dockerSHA, "worker-image": *workerImage} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if *runID <= 0 {
		return errors.New("-github-actions-run-id must be positive")
	}
	if err := verifyWorkflowExecutableVersion(*executable, *version); err != nil {
		return err
	}
	manifest := platformrelease.BundleManifest{
		SchemaVersion: 1, SetupProtocolVersion: 1, Version: *version,
		Compatibility: platformrelease.Compatibility{OS: "windows", Architecture: "amd64", DatabaseSchema: 63, DockerDesktopVersion: *dockerVersion, DockerInstallerURL: *dockerURL, DockerInstallerSHA256: strings.ToLower(*dockerSHA), WorkerImage: strings.ToLower(*workerImage)},
	}
	return platformrelease.AssembleBundle(platformrelease.BundleAssembleOptions{Output: *output, SetupExecutable: *setupExecutable, WorkflowExecutable: *executable, PayloadDirectory: *payload, Manifest: manifest})
}

func verifyWorkflowExecutableVersion(executable, expectedVersion string) error {
	// The publisher executes a freshly cross-compiled Windows binary. On the
	// Windows GitHub runner that first process start can contend with the
	// concurrent release build, so keep the bounded probe comfortably above the
	// normal cold-start budget while still failing closed.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "version").CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("read Workflow CLI published version: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("read Workflow CLI published version: %w", err)
	}
	want := "workflow " + strings.TrimSpace(expectedVersion)
	if got := strings.TrimSpace(string(output)); got != want {
		return fmt.Errorf("Workflow CLI published version %q differs from Platform Release Manifest version %q", got, want)
	}
	return nil
}
