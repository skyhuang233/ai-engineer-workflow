package platformrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
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

func TestManifestBindsCanonicalGitHubReleaseIdentityAndProvenance(t *testing.T) {
	manifest := validManifest(fixtureArtifacts())
	raw, _, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	identity := ReleaseIdentity{
		Repository:   "skyhuang233/ai-engineer-workflow",
		WorkflowPath: ".github/workflows/publish-platform.yml",
		SourceCommit: strings.Repeat("a", 40),
	}
	if _, err := VerifyManifest(raw, identity); err != nil {
		t.Fatal(err)
	}
	identity.Repository = "attacker/fork"
	if _, err := VerifyManifest(raw, identity); err == nil {
		t.Fatal("accepted manifest from a different release repository")
	}
	identity.Repository = manifest.Release.Repository
	identity.SourceCommit = strings.Repeat("b", 40)
	if _, err := VerifyManifest(raw, identity); err == nil {
		t.Fatal("accepted manifest from a different immutable release commit")
	}
}

func TestManifestRejectsRemovedSignatureMetadata(t *testing.T) {
	manifest := validManifest(fixtureArtifacts())
	raw, _, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw[:len(raw)-1], []byte(`,"signature":{"algorithm":"ecdsa-p256-sha256","key_id":"old","signature_asset":"platform-release.json.sig"}}`)...)
	if _, _, _, err := Parse(raw); err == nil {
		t.Fatal("accepted obsolete detached-signature metadata")
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
		t.Fatal("accepted release whose GitHub identity and provenance were not verified")
	}
}

func TestManifestValidationFailsClosed(t *testing.T) {
	tests := map[string]func(*Manifest){
		"missing checksum":   func(m *Manifest) { m.Artifacts[0].SHA256 = "" },
		"duplicate artifact": func(m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) },
		"extra artifact": func(m *Manifest) {
			extra := Artifact{Name: "unexpected.bin", SHA256: strings.Repeat("1", 64), Size: 1}
			m.Artifacts = append(m.Artifacts, extra)
			m.Provenance.Subjects = append(m.Provenance.Subjects, extra)
		},
		"missing scope":         func(m *Manifest) { m.PlatformSetup.Credential.RequiredScopes = []string{"repo"} },
		"mutable worker pin":    func(m *Manifest) { m.PlatformSetup.Worker.Image = "ghcr.io/owner/worker:latest" },
		"missing Docker URL":    func(m *Manifest) { m.PlatformSetup.Docker.InstallerURL = "" },
		"missing bundle digest": func(m *Manifest) { m.BundledFiles[0].SHA256 = "" },
		"development CLI version": func(m *Manifest) {
			m.Release.Version = "dev"
			m.Release.Tag = "platform-vdev"
		},
		"v-prefixed platform version": func(m *Manifest) {
			m.Release.Version = "v1.0.0"
			m.Release.Tag = "platform-v1.0.0"
		},
		"zero-padded platform version": func(m *Manifest) {
			m.Release.Version = "01.0.0"
			m.Release.Tag = "platform-v01.0.0"
		},
		"prerelease platform version": func(m *Manifest) {
			m.Release.Version = "1.0.0-rc.1"
			m.Release.Channel = "prerelease"
			m.Release.Tag = "platform-v1.0.0-rc.1"
		},
		"build platform version": func(m *Manifest) {
			m.Release.Version = "1.0.0+build.1"
			m.Release.Tag = "platform-v1.0.0+build.1"
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
	}
}

func fixtureArtifacts() map[string][]byte {
	return map[string][]byte{
		"workflow-windows-amd64.zip": []byte("package"),
		"platform-sbom.spdx.json":    []byte(`{"spdxVersion":"SPDX-2.3","name":"workflow-platform"}`),
		"platform-provenance.json":   []byte(`{"subject":"workflow-windows-amd64.zip"}`),
	}
}
