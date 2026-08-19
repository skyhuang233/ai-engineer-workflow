package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/platformrelease"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("workflow-release requires identity or assemble")
	}
	switch arguments[0] {
	case "identity":
		return runIdentity(arguments[1:], stdout)
	case "assemble":
		return runAssemble(arguments[1:])
	default:
		return fmt.Errorf("unknown workflow-release operation %q", arguments[0])
	}
}

func runIdentity(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("workflow-release identity", flag.ContinueOnError)
	toolchainPath := flags.String("toolchain", "", "toolchain configuration")
	output := flags.String("output", "", "canonical build-input JSON output")
	deployWorkerTree := flags.String("deploy-worker-tree", "", "deploy/worker Git tree")
	deliverySourceDigestTree := flags.String("delivery-source-digest-tree", "", "cmd/delivery-source-digest Git tree")
	deliverySourceTree := flags.String("delivery-source-tree", "", "internal/deliverysource Git tree")
	goModBlob := flags.String("go-mod-blob", "", "go.mod Git blob")
	goSumBlob := flags.String("go-sum-blob", "", "go.sum Git blob")
	publishWorkflowBlob := flags.String("publish-workflow-blob", "", ".github/workflows/publish-workflow.yml Git blob")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("workflow-release identity does not accept positional arguments")
	}
	for name, value := range map[string]string{
		"toolchain": *toolchainPath, "output": *output, "deploy-worker-tree": *deployWorkerTree,
		"delivery-source-digest-tree": *deliverySourceDigestTree, "delivery-source-tree": *deliverySourceTree,
		"go-mod-blob": *goModBlob, "go-sum-blob": *goSumBlob, "publish-workflow-blob": *publishWorkflowBlob,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	toolchain, err := workflowrelease.LoadToolchain(*toolchainPath)
	if err != nil {
		return err
	}
	input := workflowrelease.BuildInput{
		SchemaVersion: 1,
		GitInputs: workflowrelease.GitInputs{
			DeployWorkerTree: *deployWorkerTree, DeliverySourceDigestTree: *deliverySourceDigestTree,
			DeliverySourceTree: *deliverySourceTree, GoModBlob: *goModBlob, GoSumBlob: *goSumBlob,
			PublishWorkflowBlob: *publishWorkflowBlob,
		},
		Toolchain: toolchain.Tools(),
		Worker:    workflowrelease.BuildWorker{ImageRepository: toolchain.Worker.ImageRepository},
	}
	canonical, err := input.Canonical()
	if err != nil {
		return err
	}
	identity, err := input.Identity()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*output, canonical, 0o644); err != nil {
		return fmt.Errorf("write canonical build input: %w", err)
	}
	_, err = fmt.Fprintln(stdout, identity)
	return err
}

func runAssemble(arguments []string) error {
	flags := flag.NewFlagSet("workflow-release assemble", flag.ContinueOnError)
	configPath := flags.String("config", "", "Workflow Release configuration")
	toolchainPath := flags.String("toolchain", "", "toolchain configuration")
	buildInputPath := flags.String("build-input", "", "canonical build-input JSON")
	workflowExecutable := flags.String("workflow-exe", "", "Windows amd64 workflow.exe")
	setupExecutable := flags.String("setup-exe", "", "Windows amd64 workflow-setup.exe")
	payload := flags.String("payload", "", "staged package payload root")
	output := flags.String("output", "", "empty release output directory")
	sourceCommit := flags.String("source-commit", "", "accepted source commit")
	runID := flags.Int64("github-actions-run-id", 0, "publisher Actions run ID")
	workerImage := flags.String("worker-image", "", "immutable Worker image reference")
	sbom := flags.String("sbom", "", "generated SPDX JSON SBOM")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("workflow-release assemble does not accept positional arguments")
	}
	for name, value := range map[string]string{
		"config": *configPath, "toolchain": *toolchainPath, "build-input": *buildInputPath,
		"workflow-exe": *workflowExecutable, "setup-exe": *setupExecutable, "payload": *payload,
		"output": *output, "source-commit": *sourceCommit, "worker-image": *workerImage, "sbom": *sbom,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if *runID <= 0 {
		return errors.New("-github-actions-run-id must be positive")
	}
	config, err := workflowrelease.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	toolchain, err := workflowrelease.LoadToolchain(*toolchainPath)
	if err != nil {
		return err
	}
	buildInputRaw, err := os.ReadFile(*buildInputPath)
	if err != nil {
		return fmt.Errorf("read canonical build input: %w", err)
	}
	buildInput, err := workflowrelease.DecodeBuildInput(buildInputRaw)
	if err != nil {
		return err
	}
	canonical, err := buildInput.Canonical()
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, buildInputRaw) {
		return errors.New("build-input file is not canonical schema-1 JSON")
	}
	if buildInput.Toolchain != toolchain.Tools() || buildInput.Worker.ImageRepository != toolchain.Worker.ImageRepository {
		return errors.New("build-input identity differs from the current toolchain configuration")
	}
	if err := verifyWorkflowExecutableVersion(*workflowExecutable, config.Version); err != nil {
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
		Config: config, SourceCommit: *sourceCommit, GitHubActionsRunID: *runID, BundlePath: bundlePath,
		WorkerImage: *workerImage, BuildInput: buildInput, SBOMPath: sbomPath,
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
