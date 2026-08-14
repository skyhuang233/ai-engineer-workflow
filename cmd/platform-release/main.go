package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

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
	signingKeyPath := flags.String("signing-key", "", "ECDSA P-256 PEM private key")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	for name, value := range map[string]string{"workflow-exe": *executable, "payload": *payload, "output": *output, "version": *version, "source-commit": *sourceCommit, "docker-version": *dockerVersion, "docker-installer-url": *dockerURL, "docker-installer-sha256": *dockerSHA, "worker-image": *workerImage, "signing-key": *signingKeyPath} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if *runID <= 0 {
		return errors.New("-github-actions-run-id must be positive")
	}
	manifest, err := loadTemplate(*templatePath)
	if err != nil {
		return err
	}
	manifest.Release.Version = *version
	manifest.Release.Tag = "platform-v" + strings.TrimPrefix(*version, "v")
	manifest.Release.SourceCommit = strings.ToLower(*sourceCommit)
	manifest.Release.GitHubActionsRunID = *runID
	manifest.Provenance.SourceCommit = manifest.Release.SourceCommit
	manifest.Provenance.GitHubActionsRunID = *runID
	manifest.PlatformSetup.Docker.Version = *dockerVersion
	manifest.PlatformSetup.Docker.InstallerURL = *dockerURL
	manifest.PlatformSetup.Docker.WindowsAMD64SHA256 = strings.ToLower(*dockerSHA)
	manifest.PlatformSetup.Worker.Image = strings.ToLower(*workerImage)
	key, err := loadSigningKey(*signingKeyPath)
	if err != nil {
		return err
	}
	_, err = platformrelease.Assemble(platformrelease.AssembleOptions{OutputDirectory: *output, WorkflowExecutable: *executable, PayloadDirectory: *payload, Manifest: manifest, SigningKey: key})
	return err
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

func loadSigningKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Platform Release signing key: %w", err)
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("Platform Release signing key must be one PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Platform Release signing key: %w", err)
	}
	return key, nil
}
