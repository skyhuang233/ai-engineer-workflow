package platformrelease

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func TestBootstrapVerifiesPinnedManifestBeforePlatformDownload(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		pwsh, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validManifest(fixtureArtifacts())
	raw, _, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := Sign(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "platform-release.json")
	signaturePath := filepath.Join(directory, "platform-release.json.sig")
	publicKeyPath := filepath.Join(directory, "platform-release-public-key.pem")
	policyPath := filepath.Join(directory, "release-policy.json")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(manifestPath, raw)
	write(signaturePath, signature)
	write(publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	policy, _ := json.Marshal(map[string]any{
		"schema_version": 1, "repository": manifest.Release.Repository,
		"workflow_path": manifest.Provenance.WorkflowPath, "key_id": manifest.Signature.KeyID,
		"signature_algorithm": manifest.Signature.Algorithm, "minimum_platform_version": "0.0.0",
		"public_key_file": filepath.Base(publicKeyPath),
	})
	write(policyPath, policy)

	_, currentFile, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "skills", "setup-agent-workflow", "scripts", "verify-platform-release.ps1"))
	run := func(scriptPath string, extra ...string) (string, error) {
		arguments := []string{"-NoProfile", "-File", scriptPath, "-ManifestPath", manifestPath, "-SignaturePath", signaturePath, "-PolicyPath", policyPath, "-PublicKeyPath", publicKeyPath}
		arguments = append(arguments, extra...)
		output, err := exec.Command(pwsh, arguments...).CombinedOutput()
		return string(output), err
	}
	output, err := run(script)
	if err != nil || !strings.Contains(output, `"verified":true`) {
		t.Fatalf("verified release output = %q, %v", output, err)
	}
	hostFactsPath := filepath.Join(directory, "host-facts.json")
	planPath := filepath.Join(directory, "platform-plan.json")
	workflowHome := filepath.Join(directory, "workflow-home")
	skillsRoot := filepath.Join(directory, "codex-skills")
	hostFacts, _ := json.Marshal(map[string]any{
		"schema_version": 1, "supported_host": true, "workflow_home": workflowHome,
		"workflow": map[string]any{"installed": false}, "docker": map[string]any{"installed": true, "desktop_version": manifest.PlatformSetup.Docker.Version, "engine_os": "linux", "engine_arch": "amd64"},
		"github_credential": map[string]any{"exists": true, "verified": true, "path": filepath.Join(workflowHome, "state", "credentials", "github.pat")},
		"codex_skills_root": skillsRoot,
	})
	write(hostFactsPath, hostFacts)
	planScript := filepath.Join(filepath.Dir(script), "new-platform-bootstrap-plan.ps1")
	output, err = run(planScript, "-HostFactsPath", hostFactsPath, "-OutputPath", planPath)
	var planned struct {
		DigestSHA256  string `json:"digest_sha256"`
		CanonicalJSON string `json:"canonical_json"`
		Plan          struct {
			Preconditions []setupcontract.Precondition `json:"preconditions"`
			Effects       []struct {
				Kind       string            `json:"kind"`
				Parameters map[string]string `json:"parameters"`
			} `json:"effects"`
		} `json:"plan"`
	}
	if decodeErr := json.Unmarshal([]byte(output), &planned); decodeErr != nil {
		t.Fatalf("decode Platform Bootstrap Plan output: %v (%q)", decodeErr, output)
	}
	hasSkillBundle := false
	hasExactCLI := false
	for _, effect := range planned.Plan.Effects {
		hasSkillBundle = hasSkillBundle || effect.Kind == "workflow_skill_bundle"
		hasExactCLI = hasExactCLI || effect.Kind == "platform_cli" && effect.Parameters["sha256"] == manifest.BundledFiles[0].SHA256
	}
	parsed, canonical, digest, parseErr := setupcontract.ParsePlan([]byte(planned.CanonicalJSON))
	manifestDigest := sha256.Sum256(raw)
	if err != nil || !hasSkillBundle || !hasExactCLI || parseErr != nil || string(canonical) != planned.CanonicalJSON || digest != planned.DigestSHA256 || parsed.Preconditions[0].Kind != "platform_release" || parsed.Preconditions[0].Expected != hex.EncodeToString(manifestDigest[:]) {
		t.Fatalf("verified Platform Bootstrap Plan omitted exact Workflow Skill Bundle: %q, %v", output, err)
	}
	if _, err := os.Stat(workflowHome); !os.IsNotExist(err) {
		t.Fatalf("read-only plan generation created Workflow Home: %v", err)
	}

	write(signaturePath, append([]byte(nil), signature[:len(signature)-1]...))
	installScript := filepath.Join(filepath.Dir(script), "install-workflow-cli.ps1")
	output, err = run(installScript, "-PlanPath", filepath.Join(directory, "not-read-before-trust.json"), "-ApprovedDigest", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(output, "Platform Release signature") {
		t.Fatalf("tampered signature was not rejected before plan/download: %q, %v", output, err)
	}
}
