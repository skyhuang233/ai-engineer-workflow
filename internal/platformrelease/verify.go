package platformrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repositoryPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	versionPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	workerImagePattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9_.-]+/[a-z0-9_./-]+@sha256:[0-9a-f]{64}$`)
)

type Candidate struct {
	Manifest   Manifest
	Verified   bool
	Immutable  bool
	Draft      bool
	Prerelease bool
}

type ReleaseIdentity struct {
	Repository   string
	WorkflowPath string
	SourceCommit string
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported Platform Release Manifest schema version %d", m.SchemaVersion)
	}
	if err := ValidatePlatformVersion(m.Release.Version); err != nil {
		return err
	}
	if m.Release.Channel != "stable" {
		return errors.New("Platform Release channel must be stable")
	}
	if !repositoryPattern.MatchString(m.Release.Repository) || !shaPattern.MatchString(m.Release.SourceCommit) || m.Release.Tag != "platform-v"+m.Release.Version || m.Release.GitHubActionsRunID <= 0 {
		return errors.New("Platform Release identity is incomplete")
	}
	if m.BootstrapContract.MinimumSchema <= 0 || m.BootstrapContract.MaximumSchema < m.BootstrapContract.MinimumSchema {
		return errors.New("bootstrap contract schema range is invalid")
	}
	if err := m.PlatformSetup.validate(); err != nil {
		return err
	}
	if err := validateArtifacts(m.Artifacts, true); err != nil {
		return err
	}
	if len(m.BundledFiles) == 0 {
		return errors.New("Platform Release must bind bundled files")
	}
	bundledPaths := make(map[string]struct{}, len(m.BundledFiles))
	for _, file := range m.BundledFiles {
		clean := path.Clean(file.Path)
		if clean != file.Path || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || !sha256Pattern.MatchString(file.SHA256) {
			return fmt.Errorf("bundled file %q is invalid", file.Path)
		}
		if _, duplicate := bundledPaths[file.Path]; duplicate {
			return fmt.Errorf("duplicate bundled file %q", file.Path)
		}
		bundledPaths[file.Path] = struct{}{}
	}
	if m.Provenance.Repository != m.Release.Repository || m.Provenance.SourceCommit != m.Release.SourceCommit || m.Provenance.GitHubActionsRunID != m.Release.GitHubActionsRunID || m.Provenance.WorkflowPath != ".github/workflows/publish-platform.yml" || m.Provenance.BuilderID != "github-actions" {
		return errors.New("Platform Release provenance does not match release identity")
	}
	if err := validateArtifacts(m.Provenance.Subjects, true); err != nil {
		return fmt.Errorf("invalid provenance subjects: %w", err)
	}
	if !sameArtifacts(m.Artifacts, m.Provenance.Subjects) {
		return errors.New("Platform Release provenance subjects do not cover exact release artifacts")
	}
	return nil
}

// ValidatePlatformVersion accepts only the canonical bare SemVer core used by
// every Platform Release identity boundary.
func ValidatePlatformVersion(value string) error {
	parts := versionPattern.FindStringSubmatch(value)
	if parts == nil {
		return errors.New("Platform Release version must be a bare semantic version core (X.Y.Z) without leading zeros")
	}
	for _, part := range parts[1:] {
		if _, err := strconv.ParseUint(part, 10, 31); err != nil {
			return errors.New("Platform Release version components must fit the signed 32-bit range")
		}
	}
	return nil
}

func (c PlatformSetupContract) validate() error {
	if c.WorkflowHomeDefault != `%LOCALAPPDATA%\AgentWorkflow` {
		return errors.New("Workflow Home default is invalid")
	}
	if c.Credential.Kind != "classic-pat" || c.Credential.OwnerBinding != "single-owner" || c.Credential.PlaintextRelativePath != `state\credentials\github.pat` || !equalStringSet(c.Credential.RequiredScopes, []string{"repo", "workflow"}) {
		return errors.New("Control Plane credential contract must be the owner-bound classic PAT with repo and workflow scopes")
	}
	installerURL, err := url.Parse(c.Docker.InstallerURL)
	if err != nil || installerURL.Scheme != "https" || installerURL.Host == "" || strings.TrimSpace(c.Docker.Version) == "" || !sha256Pattern.MatchString(c.Docker.WindowsAMD64SHA256) {
		return errors.New("Docker Desktop dependency must declare an exact HTTPS installer and SHA-256")
	}
	if !workerImagePattern.MatchString(c.Worker.Image) {
		return errors.New("Worker image must be pinned by immutable GHCR digest")
	}
	if strings.TrimSpace(c.SkillBundle.Version) == "" || c.SkillBundle.InstallScope != "user" || len(c.SkillBundle.ManagedSkills) == 0 {
		return errors.New("Workflow Skill Bundle contract is incomplete")
	}
	if strings.TrimSpace(c.RepositoryContract.Version) == "" || c.RepositoryContract.ManifestPath != ".workflow/repository.json" || c.RepositoryContract.CheckName != "workflow-contract" {
		return errors.New("Repository Contract pin is incomplete")
	}
	if len(c.RepositoryContract.Labels) == 0 {
		return errors.New("Repository Contract label vocabulary is empty")
	}
	labels := map[string]struct{}{}
	for _, label := range c.RepositoryContract.Labels {
		name := strings.TrimSpace(label.Name)
		if name == "" || !regexp.MustCompile(`^[0-9a-fA-F]{6}$`).MatchString(label.Color) || strings.TrimSpace(label.Description) == "" {
			return fmt.Errorf("Repository Contract label %q is invalid", label.Name)
		}
		if _, duplicate := labels[strings.ToLower(name)]; duplicate {
			return fmt.Errorf("duplicate Repository Contract label %q", label.Name)
		}
		labels[strings.ToLower(name)] = struct{}{}
	}
	return nil
}

func (c PlatformSetupContract) Validate() error { return c.validate() }

func validateArtifacts(artifacts []Artifact, requireCore bool) error {
	if len(artifacts) == 0 {
		return errors.New("Platform Release artifacts are required")
	}
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if path.Base(artifact.Name) != artifact.Name || artifact.Name == "." || !sha256Pattern.MatchString(artifact.SHA256) || artifact.Size <= 0 {
			return fmt.Errorf("artifact %q is incomplete", artifact.Name)
		}
		if _, duplicate := seen[artifact.Name]; duplicate {
			return fmt.Errorf("duplicate artifact %q", artifact.Name)
		}
		seen[artifact.Name] = struct{}{}
	}
	if requireCore {
		requiredArtifacts := []string{"workflow-windows-amd64.zip", "platform-sbom.spdx.json", "platform-provenance.json"}
		if len(seen) != len(requiredArtifacts) {
			return errors.New("Platform Release artifact set must exactly match required artifacts")
		}
		for _, required := range requiredArtifacts {
			if _, ok := seen[required]; !ok {
				return fmt.Errorf("Platform Release lacks required artifact %q", required)
			}
		}
	}
	return nil
}

func (m Manifest) VerifyArtifacts(actual map[string][]byte) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if len(actual) != len(m.Artifacts) {
		return errors.New("artifact set does not exactly match Platform Release Manifest")
	}
	for _, artifact := range m.Artifacts {
		data, ok := actual[artifact.Name]
		if !ok || int64(len(data)) != artifact.Size {
			return fmt.Errorf("artifact %q size does not match manifest", artifact.Name)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return fmt.Errorf("artifact %q SHA-256 does not match manifest", artifact.Name)
		}
	}
	return nil
}

func SelectLatestStable(candidates []Candidate, bootstrapSchema int) (Manifest, error) {
	compatible := make([]Manifest, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Verified || candidate.Draft || candidate.Prerelease || !candidate.Immutable || candidate.Manifest.Release.Channel != "stable" || !candidate.Manifest.BootstrapContract.Supports(bootstrapSchema) {
			continue
		}
		if err := candidate.Manifest.Validate(); err != nil {
			continue
		}
		if _, stable := parseSemver(candidate.Manifest.Release.Version); !stable {
			continue
		}
		compatible = append(compatible, candidate.Manifest)
	}
	if len(compatible) == 0 {
		return Manifest{}, errors.New("no immutable compatible stable Platform Release")
	}
	sort.Slice(compatible, func(i, j int) bool {
		return compareSemver(compatible[i].Release.Version, compatible[j].Release.Version) > 0
	})
	return compatible[0], nil
}

// VerifyManifest binds release metadata fetched from GitHub to the canonical
// repository, immutable source commit, and publishing workflow expected by the
// caller. Manifest validation separately binds every artifact digest and the
// provenance subjects to that same release identity.
func VerifyManifest(raw []byte, identity ReleaseIdentity) (Manifest, error) {
	if !repositoryPattern.MatchString(identity.Repository) || !shaPattern.MatchString(identity.SourceCommit) || identity.WorkflowPath != ".github/workflows/publish-platform.yml" {
		return Manifest{}, errors.New("Platform Release identity is invalid")
	}
	manifest, _, _, err := Parse(raw)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Release.Repository != identity.Repository || manifest.Release.SourceCommit != identity.SourceCommit || manifest.Provenance.Repository != identity.Repository || manifest.Provenance.SourceCommit != identity.SourceCommit || manifest.Provenance.WorkflowPath != identity.WorkflowPath {
		return Manifest{}, errors.New("Platform Release does not match GitHub release identity")
	}
	return manifest, nil
}

func sameArtifacts(left, right []Artifact) bool {
	if len(left) != len(right) {
		return false
	}
	copyLeft := append([]Artifact(nil), left...)
	copyRight := append([]Artifact(nil), right...)
	sort.Slice(copyLeft, func(i, j int) bool { return copyLeft[i].Name < copyLeft[j].Name })
	sort.Slice(copyRight, func(i, j int) bool { return copyRight[i].Name < copyRight[j].Name })
	for index := range copyLeft {
		if copyLeft[index] != copyRight[index] {
			return false
		}
	}
	return true
}

func equalStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	for index := range a {
		if a[index] != b[index] || (index > 0 && a[index] == a[index-1]) {
			return false
		}
	}
	return true
}

func parseSemver(value string) ([3]int, bool) {
	match := versionPattern.FindStringSubmatch(value)
	if match == nil {
		return [3]int{}, false
	}
	var parsed [3]int
	for index := range parsed {
		parsed[index], _ = strconv.Atoi(match[index+1])
	}
	return parsed, true
}

func compareSemver(left, right string) int {
	a, _ := parseSemver(left)
	b, _ := parseSemver(right)
	for index := range a {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	return 0
}
