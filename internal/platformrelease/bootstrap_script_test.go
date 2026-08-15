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
	manifest.PlatformSetup.SkillBundle.ManagedSkills = []string{"implement"}
	skillBody := []byte("# implement\n")
	skillDigest := sha256.Sum256(skillBody)
	for index := range manifest.BundledFiles {
		if manifest.BundledFiles[index].Path == "skills/implement/SKILL.md" {
			manifest.BundledFiles[index].SHA256 = hex.EncodeToString(skillDigest[:])
		}
	}
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
		"github_credential": map[string]any{"exists": true, "verified": true, "owner": "owner", "scopes": []string{"repo", "workflow"}, "path": filepath.Join(workflowHome, "state", "credentials", "github.pat")},
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
	var projected struct {
		Projection string `json:"projection"`
	}
	if err := json.Unmarshal([]byte(output), &projected); err != nil || !strings.Contains(projected.Projection, "Authorized effects:") || strings.HasPrefix(strings.TrimSpace(projected.Projection), "{") {
		t.Fatalf("Platform projection is not human-readable: %#v, %v", projected, err)
	}
	if _, err := os.Stat(workflowHome); !os.IsNotExist(err) {
		t.Fatalf("read-only plan generation created Workflow Home: %v", err)
	}

	// Exact readback facts must produce a true no-op rather than another empty
	// approval cycle. The no-op envelope remains digest-bound for audit display.
	if err := os.MkdirAll(filepath.Join(skillsRoot, "implement"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workflowHome, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(skillsRoot, "implement", "SKILL.md"), skillBody)
	ownerJSON, _ := json.Marshal(map[string]string{"owner": "agent-workflow-platform", "version": manifest.PlatformSetup.SkillBundle.Version})
	write(filepath.Join(skillsRoot, "implement", ".agent-workflow-owner.json"), ownerJSON)
	write(filepath.Join(workflowHome, "config", "workflow-skills.owner.json"), ownerJSON)
	contractDigest := parsed.Preconditions[1].Expected
	noOpState := map[string]any{
		"schema_version": 1, "supported_host": true, "workflow_home": workflowHome,
		"workflow":          map[string]any{"installed": true, "owned": true, "path_reconciled": true, "version": manifest.Release.Version, "sha256": manifest.BundledFiles[0].SHA256},
		"platform":          map[string]any{"installation_recorded": true, "version": manifest.Release.Version, "release_manifest_digest": hex.EncodeToString(manifestDigest[:]), "platform_setup_contract_digest": contractDigest},
		"docker":            map[string]any{"installed": true, "desktop_version": manifest.PlatformSetup.Docker.Version, "engine_os": "linux", "engine_arch": "amd64"},
		"github_credential": map[string]any{"exists": true, "verified": true, "owner": "owner", "scopes": []string{"repo", "workflow"}, "path": filepath.Join(workflowHome, "state", "credentials", "github.pat")},
		"control_plane":     map[string]any{"state": "ready", "runtime": map[string]any{"platform_version": manifest.Release.Version}}, "codex_skills_root": skillsRoot,
	}
	noOpFacts, _ := json.Marshal(noOpState)
	write(hostFactsPath, noOpFacts)
	output, err = run(planScript, "-HostFactsPath", hostFactsPath, "-OutputPath", planPath)
	var noOp struct {
		Status        string `json:"status"`
		DigestSHA256  string `json:"digest_sha256"`
		CanonicalJSON string `json:"canonical_json"`
		Projection    string `json:"projection"`
		Plan          struct {
			Effects []json.RawMessage `json:"effects"`
		} `json:"plan"`
	}
	if decodeErr := json.Unmarshal([]byte(output), &noOp); err != nil || decodeErr != nil || noOp.Status != "ready" || len(noOp.Plan.Effects) != 0 || noOp.DigestSHA256 == "" || noOp.CanonicalJSON == "" || !strings.Contains(noOp.Projection, "Authorized effects:") {
		t.Fatalf("exact platform readback did not produce digest-bound no-op: %q, %v, %v", output, err, decodeErr)
	}

	assertSingleRepair := func(name, wantKind string, extra ...string) {
		t.Helper()
		output, runErr := run(planScript, append([]string{"-HostFactsPath", hostFactsPath, "-OutputPath", planPath}, extra...)...)
		var repair struct {
			CanonicalJSON string `json:"canonical_json"`
			Plan          struct {
				Effects []struct {
					Kind       string            `json:"kind"`
					Action     string            `json:"action"`
					Parameters map[string]string `json:"parameters"`
				} `json:"effects"`
			} `json:"plan"`
		}
		decodeErr := json.Unmarshal([]byte(output), &repair)
		_, _, _, parseErr := setupcontract.ParsePlan([]byte(repair.CanonicalJSON))
		if runErr != nil || decodeErr != nil || parseErr != nil || len(repair.Plan.Effects) != 1 || repair.Plan.Effects[0].Kind != wantKind || repair.Plan.Effects[0].Parameters["release_manifest_digest"] != hex.EncodeToString(manifestDigest[:]) || repair.Plan.Effects[0].Parameters["platform_setup_contract_digest"] != contractDigest || repair.Plan.Effects[0].Parameters["workflow_cli_sha256"] != manifest.BundledFiles[0].SHA256 {
			t.Fatalf("%s repair was not independently release-bound: %q, %v, %v, %v", name, output, runErr, decodeErr, parseErr)
		}
	}
	noOpState["workflow"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	cliOnlyFacts, _ := json.Marshal(noOpState)
	write(hostFactsPath, cliOnlyFacts)
	assertSingleRepair("CLI-only", "platform_cli")
	noOpState["workflow"].(map[string]any)["sha256"] = manifest.BundledFiles[0].SHA256
	noOpState["control_plane"] = map[string]any{"state": "stopped", "runtime": map[string]any{"platform_version": manifest.Release.Version}}
	cpOnlyFacts, _ := json.Marshal(noOpState)
	write(hostFactsPath, cpOnlyFacts)
	assertSingleRepair("Control-Plane-only", "control_plane")
	noOpState["control_plane"] = map[string]any{"state": "ready", "runtime": map[string]any{"platform_version": manifest.Release.Version}}
	ownerFacts, _ := json.Marshal(noOpState)
	write(hostFactsPath, ownerFacts)
	assertSingleRepair("wrong-owner PAT", "github_pat", "-GitHubOwner", "different-owner")

	installScript := filepath.Join(filepath.Dir(script), "install-workflow-cli.ps1")
	tampered := parsed
	tampered.Preconditions = append([]setupcontract.Precondition(nil), parsed.Preconditions...)
	tampered.Preconditions[0].Expected = strings.Repeat("0", 64)
	tamperedRaw, _ := json.Marshal(tampered)
	_, tamperedCanonical, tamperedDigest, err := setupcontract.ParsePlan(tamperedRaw)
	if err != nil {
		t.Fatal(err)
	}
	tamperedEnvelope, _ := json.Marshal(map[string]any{"status": "plan_required", "digest_sha256": tamperedDigest, "canonical_json": string(tamperedCanonical), "plan": tampered, "projection": "tampered"})
	tamperedPlanPath := filepath.Join(directory, "tampered-plan.json")
	write(tamperedPlanPath, tamperedEnvelope)
	output, err = run(installScript, "-PlanPath", tamperedPlanPath, "-ApprovedDigest", tamperedDigest)
	if err == nil || !strings.Contains(output, "does not bind the verified Platform Release and contract") {
		t.Fatalf("manifest/precondition mismatch reached download or apply: %q, %v", output, err)
	}

	write(signaturePath, append([]byte(nil), signature[:len(signature)-1]...))
	output, err = run(installScript, "-PlanPath", filepath.Join(directory, "not-read-before-trust.json"), "-ApprovedDigest", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(output, "Platform Release signature") {
		t.Fatalf("tampered signature was not rejected before plan/download: %q, %v", output, err)
	}
}
