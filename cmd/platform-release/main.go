package main

import (
	"context"
	"encoding/json"
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
	templatePath := flags.String("template", "deploy/platform/release-manifest.json", "Platform Release Manifest source template")
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
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("platform-release does not accept positional arguments")
	}
	for name, value := range map[string]string{"workflow-exe": *executable, "payload": *payload, "output": *output, "version": *version, "source-commit": *sourceCommit, "docker-version": *dockerVersion, "docker-installer-url": *dockerURL, "docker-installer-sha256": *dockerSHA, "worker-image": *workerImage} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if *runID <= 0 {
		return errors.New("-github-actions-run-id must be positive")
	}
	if err := platformrelease.ValidatePlatformVersion(*version); err != nil {
		return fmt.Errorf("-version: %w", err)
	}
	if err := verifyWorkflowExecutableVersion(*executable, *version); err != nil {
		return err
	}
	manifest, err := loadTemplate(*templatePath)
	if err != nil {
		return err
	}
	manifest.Release.Version = *version
	manifest.Release.Tag = "platform-v" + *version
	manifest.Release.SourceCommit = strings.ToLower(*sourceCommit)
	manifest.Release.GitHubActionsRunID = *runID
	manifest.Provenance.SourceCommit = manifest.Release.SourceCommit
	manifest.Provenance.GitHubActionsRunID = *runID
	manifest.PlatformSetup.Docker.Version = *dockerVersion
	manifest.PlatformSetup.Docker.InstallerURL = *dockerURL
	manifest.PlatformSetup.Docker.WindowsAMD64SHA256 = strings.ToLower(*dockerSHA)
	manifest.PlatformSetup.Worker.Image = strings.ToLower(*workerImage)
	_, err = platformrelease.Assemble(platformrelease.AssembleOptions{OutputDirectory: *output, WorkflowExecutable: *executable, PayloadDirectory: *payload, Manifest: manifest})
	return err
}

func verifyWorkflowExecutableVersion(executable, expectedVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

func loadTemplate(path string) (platformrelease.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return platformrelease.Manifest{}, fmt.Errorf("open Platform Release template: %w", err)
	}
	defer file.Close()
	var manifest platformrelease.Manifest
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return platformrelease.Manifest{}, fmt.Errorf("decode Platform Release template: %w", err)
	}
	return manifest, nil
}
