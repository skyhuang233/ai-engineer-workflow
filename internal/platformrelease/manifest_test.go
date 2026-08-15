package platformrelease

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func TestManifestRoundTripAndArtifactVerification(t *testing.T) {
	artifacts := map[string][]byte{
		"workflow-windows-amd64.zip": []byte("package"),
		"platform-sbom.spdx.json":    []byte(`{"spdxVersion":"SPDX-2.3","name":"workflow-platform"}`),
		"platform-provenance.json":   []byte(`{"subject":"workflow-windows-amd64.zip"}`),
	}
	manifest := validManifest(artifacts)
	raw, digest, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	loaded, canonicalAgain, digestAgain, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Release.Version != manifest.Release.Version || digestAgain != digest || string(canonicalAgain) != string(raw) {
		t.Fatal("Platform Release Manifest authority changed after round trip")
	}
	if err := loaded.VerifyArtifacts(artifacts); err != nil {
		t.Fatal(err)
	}
	artifacts["workflow-windows-amd64.zip"] = []byte("tampered")
	if err := loaded.VerifyArtifacts(artifacts); err == nil {
		t.Fatal("accepted corrupt platform artifact")
	}
}

func TestSignedManifestBindsTrustPolicyAndProvenance(t *testing.T) {
	manifest := validManifest(fixtureArtifacts())
	raw, _, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := Sign(raw, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	policy := TrustPolicy{
		Repository:   "skyhuang233/ai-engineer-workflow",
		WorkflowPath: ".github/workflows/publish-platform.yml",
		KeyID:        "platform-release-2026",
	}
	if _, err := VerifySignedManifest(raw, signature, &privateKey.PublicKey, policy); err != nil {
		t.Fatal(err)
	}
	policy.Repository = "attacker/fork"
	if _, err := VerifySignedManifest(raw, signature, &privateKey.PublicKey, policy); err == nil {
		t.Fatal("accepted manifest from a different release repository")
	}
	raw[len(raw)-1] ^= 1
	if _, err := VerifySignedManifest(raw, signature, &privateKey.PublicKey, TrustPolicy{
		Repository: "skyhuang233/ai-engineer-workflow", WorkflowPath: ".github/workflows/publish-platform.yml", KeyID: "platform-release-2026",
	}); err == nil {
		t.Fatal("accepted tampered signed manifest")
	}
}

func TestSelectLatestStableRequiresCompatibleVerifiedRelease(t *testing.T) {
	base := validManifest(fixtureArtifacts())
	older := base
	older.Release.Version = "0.9.0"
	prerelease := base
	prerelease.Release.Version = "2.0.0-rc.1"
	prerelease.Release.Channel = "prerelease"
	incompatible := base
	incompatible.Release.Version = "1.5.0"
	incompatible.BootstrapContract.MinimumSchema = 3
	incompatible.BootstrapContract.MaximumSchema = 4

	selected, err := SelectLatestStable([]Candidate{
		{Manifest: prerelease, Verified: true, Immutable: true},
		{Manifest: older, Verified: true, Immutable: true},
		{Manifest: incompatible, Verified: true, Immutable: true},
		{Manifest: base, Verified: true, Immutable: true},
	}, setupcontract.SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Release.Version != "1.0.0" {
		t.Fatalf("selected version = %s", selected.Release.Version)
	}
	if _, err := SelectLatestStable([]Candidate{{Manifest: base, Verified: true, Immutable: false}}, setupcontract.SchemaVersion); err == nil {
		t.Fatal("accepted mutable release")
	}
	if _, err := SelectLatestStable([]Candidate{{Manifest: base, Immutable: true}}, setupcontract.SchemaVersion); err == nil {
		t.Fatal("accepted release whose signature and provenance were not verified")
	}
}

func TestManifestValidationFailsClosed(t *testing.T) {
	tests := map[string]func(*Manifest){
		"missing checksum":      func(m *Manifest) { m.Artifacts[0].SHA256 = "" },
		"duplicate artifact":    func(m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) },
		"missing scope":         func(m *Manifest) { m.PlatformSetup.Credential.RequiredScopes = []string{"repo"} },
		"mutable worker pin":    func(m *Manifest) { m.PlatformSetup.Worker.Image = "ghcr.io/owner/worker:latest" },
		"missing Docker URL":    func(m *Manifest) { m.PlatformSetup.Docker.InstallerURL = "" },
		"missing bundle digest": func(m *Manifest) { m.BundledFiles[0].SHA256 = "" },
		"development CLI version": func(m *Manifest) {
			m.Release.Version = "dev"
			m.Release.Tag = "platform-vdev"
		},
		"provenance mismatch": func(m *Manifest) { m.Provenance.SourceCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := validManifest(fixtureArtifacts())
			mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("accepted incomplete Platform Release Manifest")
			}
		})
	}
}

func validManifest(artifactData map[string][]byte) Manifest {
	artifacts := make([]Artifact, 0, len(artifactData))
	for _, name := range []string{"platform-provenance.json", "platform-sbom.spdx.json", "workflow-windows-amd64.zip"} {
		data := artifactData[name]
		sum := sha256.Sum256(data)
		artifacts = append(artifacts, Artifact{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
	}
	return Manifest{
		SchemaVersion:     1,
		Release:           ReleaseMetadata{Version: "1.0.0", Channel: "stable", Repository: "skyhuang233/ai-engineer-workflow", Tag: "platform-v1.0.0", SourceCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GitHubActionsRunID: 42},
		BootstrapContract: SchemaRange{MinimumSchema: 1, MaximumSchema: 2},
		PlatformSetup: PlatformSetupContract{
			WorkflowHomeDefault: `%LOCALAPPDATA%\AgentWorkflow`,
			Credential:          CredentialContract{Kind: "classic-pat", RequiredScopes: []string{"repo", "workflow"}, OwnerBinding: "single-owner", PlaintextRelativePath: `state\credentials\github.pat`},
			Docker:              DockerDependency{Version: "4.45.0", InstallerURL: "https://desktop.docker.com/win/main/amd64/Docker%20Desktop%20Installer.exe", WindowsAMD64SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
			Worker:              WorkerPin{Image: "ghcr.io/skyhuang233/workflow-worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
			SkillBundle:         SkillBundleContract{Version: "1.0.0", InstallScope: "user", ManagedSkills: []string{"implement", "review"}},
			RepositoryContract:  RepositoryContractPin{Version: "1", ManifestPath: ".workflow/repository.json", CheckName: "workflow-contract", Labels: []RepositoryLabel{{Name: "workflow:plan", Color: "0e8a16", Description: "delivery plan"}}},
		},
		Artifacts:    artifacts,
		BundledFiles: []BundledFile{{Path: "bin/workflow.exe", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, {Path: "skills/implement/SKILL.md", SHA256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}, {Path: "repository-contract/AGENTS.block.md", SHA256: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}},
		Provenance:   Provenance{Repository: "skyhuang233/ai-engineer-workflow", SourceCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkflowPath: ".github/workflows/publish-platform.yml", GitHubActionsRunID: 42, BuilderID: "github-actions", Subjects: artifacts},
		Signature:    SignatureMetadata{Algorithm: "ecdsa-p256-sha256", KeyID: "platform-release-2026", SignatureAsset: "platform-release.json.sig"},
	}
}

func fixtureArtifacts() map[string][]byte {
	return map[string][]byte{
		"workflow-windows-amd64.zip": []byte("package"),
		"platform-sbom.spdx.json":    []byte(`{"spdxVersion":"SPDX-2.3","name":"workflow-platform"}`),
		"platform-provenance.json":   []byte(`{"subject":"workflow-windows-amd64.zip"}`),
	}
}
