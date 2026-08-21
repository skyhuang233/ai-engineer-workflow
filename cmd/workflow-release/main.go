package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("workflow-release requires assemble")
	}
	switch arguments[0] {
	case "assemble":
		return runAssemble(arguments[1:])
	default:
		return fmt.Errorf("unknown workflow-release operation %q", arguments[0])
	}
}

func runAssemble(arguments []string) error {
	flags := flag.NewFlagSet("workflow-release assemble", flag.ContinueOnError)
	configPath := flags.String("config", "", "Workflow Release configuration")
	toolchainPath := flags.String("toolchain", "", "toolchain configuration")
	workflowExecutable := flags.String("workflow-exe", "", "Windows amd64 workflow.exe")
	workflowVersionExecutable := flags.String("workflow-version-exe", "", "host-native workflow executable used to verify the version")
	setupExecutable := flags.String("setup-exe", "", "Windows amd64 workflow-setup.exe")
	payload := flags.String("payload", "", "staged package payload root")
	output := flags.String("output", "", "empty release output directory")
	sourceCommit := flags.String("candidate-source-commit", "", "qualified candidate source commit")
	runID := flags.Int64("qualification-run-id", 0, "qualification Actions run ID")
	workerImage := flags.String("worker-image", "", "immutable Worker image reference")
	sbom := flags.String("sbom", "", "generated SPDX JSON SBOM")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("workflow-release assemble does not accept positional arguments")
	}
	for name, value := range map[string]string{
		"config": *configPath, "toolchain": *toolchainPath,
		"workflow-exe": *workflowExecutable, "setup-exe": *setupExecutable, "payload": *payload,
		"output": *output, "candidate-source-commit": *sourceCommit, "worker-image": *workerImage, "sbom": *sbom,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if *runID <= 0 {
		return errors.New("-qualification-run-id must be positive")
	}
	config, err := workflowrelease.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	toolchain, err := workflowrelease.LoadToolchain(*toolchainPath)
	if err != nil {
		return err
	}
	versionExecutable := strings.TrimSpace(*workflowVersionExecutable)
	if versionExecutable == "" {
		versionExecutable = *workflowExecutable
	}
	if err := verifyWorkflowExecutableVersion(versionExecutable, config.Version); err != nil {
		return err
	}
	if err := requireEmptyDirectory(*output); err != nil {
		return err
	}
	bundlePath := filepath.Join(*output, workflowrelease.BundleAssetName)
	bundleManifest := platformrelease.BundleManifest{
		SchemaVersion: 1, SetupProtocolVersion: 1, Version: config.Version,
		Compatibility: platformrelease.Compatibility{
			OS: "windows", Architecture: "amd64", DatabaseSchema: 63, WorkerImage: *workerImage,
			DockerDesktopVersion: config.DockerDesktop.Version, DockerInstallerURL: config.DockerDesktop.InstallerURL,
			DockerInstallerSHA256: config.DockerDesktop.WindowsAMD64SHA256,
		},
	}
	if err := platformrelease.AssembleBundle(platformrelease.BundleAssembleOptions{
		Output: bundlePath, SetupExecutable: *setupExecutable, WorkflowExecutable: *workflowExecutable,
		PayloadDirectory: *payload, Manifest: bundleManifest,
	}); err != nil {
		return fmt.Errorf("assemble Workflow Bundle: %w", err)
	}
	sbomRaw, err := os.ReadFile(*sbom)
	if err != nil {
		return fmt.Errorf("read Worker SBOM: %w", err)
	}
	sbomPath := filepath.Join(*output, workflowrelease.SBOMAssetName)
	if err := os.WriteFile(sbomPath, sbomRaw, 0o644); err != nil {
		return fmt.Errorf("stage Worker SBOM: %w", err)
	}
	manifest, err := workflowrelease.CreateManifest(workflowrelease.ManifestOptions{
		Config: config, CandidateSourceCommit: *sourceCommit, QualificationRunID: *runID, BundlePath: bundlePath,
		WorkerImage: *workerImage, Tools: toolchain.Tools(), SBOMPath: sbomPath,
	})
	if err != nil {
		return err
	}
	manifestRaw, err := manifest.Canonical()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*output, workflowrelease.ManifestAssetName), manifestRaw, 0o644); err != nil {
		return fmt.Errorf("write Workflow Release Manifest: %w", err)
	}
	return nil
}

func requireEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.MkdirAll(path, 0o755)
	}
	if len(entries) != 0 {
		return errors.New("release output directory must be empty")
	}
	return nil
}

func verifyWorkflowExecutableVersion(executable, expectedVersion string) error {
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
		return fmt.Errorf("Workflow CLI published version %q differs from Workflow Release version %q", got, want)
	}
	return nil
}
