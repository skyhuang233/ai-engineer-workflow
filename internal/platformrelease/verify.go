package platformrelease

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	versionPattern     = regexp.MustCompile(`^(?:v)?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?$`)
	workerImagePattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9_.-]+/[a-z0-9_./-]+@sha256:[0-9a-f]{64}$`)
)

type Candidate struct {
	Manifest   Manifest
	Verified   bool
	Immutable  bool
	Draft      bool
	Prerelease bool
}

type TrustPolicy struct {
	Repository   string
	WorkflowPath string
	KeyID        string
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported Platform Release Manifest schema version %d", m.SchemaVersion)
	}
	versionMatch := versionPattern.FindStringSubmatch(m.Release.Version)
	if versionMatch == nil {
		return errors.New("Platform Release version must be semantic")
	}
	if (m.Release.Channel == "stable") == (versionMatch[4] != "") {
		return errors.New("Platform Release channel does not match semantic version")
	}
	if m.Release.Channel != "stable" && m.Release.Channel != "prerelease" {
		return errors.New("Platform Release channel is invalid")
	}
	if !repositoryPattern.MatchString(m.Release.Repository) || !shaPattern.MatchString(m.Release.SourceCommit) || m.Release.Tag != "platform-v"+strings.TrimPrefix(m.Release.Version, "v") || m.Release.GitHubActionsRunID <= 0 {
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
	if m.Signature.Algorithm != "ecdsa-p256-sha256" || strings.TrimSpace(m.Signature.KeyID) == "" || m.Signature.SignatureAsset != "platform-release.json.sig" {
		return errors.New("Platform Release signature metadata is invalid")
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
		for _, required := range []string{"workflow-windows-amd64.zip", "platform-sbom.spdx.json", "platform-provenance.json"} {
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

func Sign(raw []byte, privateKey *ecdsa.PrivateKey) ([]byte, error) {
	if privateKey == nil || !isP256(privateKey.Curve) {
		return nil, errors.New("Platform Release signing key must use ECDSA P-256")
	}
	canonical, _, err := canonicalManifest(raw)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	return ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
}

func VerifySignedManifest(raw, signature []byte, publicKey *ecdsa.PublicKey, policy TrustPolicy) (Manifest, error) {
	manifest, canonical, _, err := Parse(raw)
	if err != nil {
		return Manifest{}, err
	}
	if publicKey == nil || !isP256(publicKey.Curve) || !repositoryPattern.MatchString(policy.Repository) || policy.WorkflowPath == "" || policy.KeyID == "" {
		return Manifest{}, errors.New("Platform Release trust policy is invalid")
	}
	if manifest.Release.Repository != policy.Repository || manifest.Provenance.Repository != policy.Repository || manifest.Provenance.WorkflowPath != policy.WorkflowPath || manifest.Signature.KeyID != policy.KeyID {
		return Manifest{}, errors.New("Platform Release does not match trust policy")
	}
	digest := sha256.Sum256(canonical)
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return Manifest{}, errors.New("Platform Release signature is invalid")
	}
	return manifest, nil
}

func isP256(curve elliptic.Curve) bool {
	return curve != nil && curve.Params().Name == elliptic.P256().Params().Name && curve.Params().BitSize == 256
}

func canonicalManifest(raw []byte) ([]byte, string, error) {
	_, canonical, digest, err := Parse(raw)
	return canonical, digest, err
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
	return parsed, match[4] == ""
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
