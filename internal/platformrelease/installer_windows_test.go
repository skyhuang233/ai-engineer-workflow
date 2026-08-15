package platformrelease

import (
	"archive/zip"
	"bytes"
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

func TestFreshBootstrapInstallerForwardsPATOnStdinWithoutLeakingItOnWindowsPowerShell51(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is the supported bootstrap shell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	directory := t.TempDir()
	fakeSource := filepath.Join(directory, "fake-workflow.go")
	fakeExecutable := filepath.Join(directory, "workflow.exe")
	fakeProgram := `package main
import ("encoding/json"; "io"; "os"; "strings")
func main() {
 args := strings.Join(os.Args[1:], " ")
 calls, _ := os.OpenFile(os.Getenv("WORKFLOW_TEST_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); if calls != nil { _, _ = calls.WriteString(args+"\n"); _ = calls.Close() }
 if strings.HasPrefix(args, "setup apply ") { input, _ := io.ReadAll(os.Stdin); _ = os.WriteFile(os.Getenv("WORKFLOW_TEST_STDIN"), input, 0600); _ = os.WriteFile(os.Getenv("WORKFLOW_TEST_ARGS"), []byte(strings.Join(os.Args[1:], "\n")), 0600); return }
 digest := os.Getenv("WORKFLOW_TEST_CP_DIGEST")
 if strings.HasPrefix(args, "setup inspect-platform ") { _ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status":"ready", "result":map[string]any{"platform":map[string]any{"installation_recorded":true,"version":os.Getenv("WORKFLOW_TEST_RELEASE_VERSION"),"release_manifest_digest":os.Getenv("WORKFLOW_TEST_MANIFEST_DIGEST"),"platform_setup_contract_digest":os.Getenv("WORKFLOW_TEST_CONTRACT_DIGEST"),"workflow_cli_sha256":os.Getenv("WORKFLOW_TEST_CLI_DIGEST"),"release_bundled_files_json":os.Getenv("WORKFLOW_TEST_BUNDLE_JSON"),"release_bundled_files_digest":os.Getenv("WORKFLOW_TEST_BUNDLE_DIGEST"),"control_plane_plan_digest_sha256":digest},"workflow_cli":map[string]any{"verified":true}}}); return }
 if strings.HasPrefix(args, "status ") { _ = json.NewEncoder(os.Stdout).Encode(map[string]any{"state":"ready", "runtime":map[string]any{"platform_version":os.Getenv("WORKFLOW_TEST_RELEASE_VERSION"),"approved_platform_bootstrap_plan_digest_sha256":digest}}); return }
 os.Exit(2)
}
`
	if err := os.WriteFile(fakeSource, []byte(fakeProgram), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", fakeExecutable, fakeSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake workflow.exe: %v (%s)", err, output)
	}
	executableBytes, err := os.ReadFile(fakeExecutable)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(directory, "workflow-windows-amd64.zip")
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveWriter := zip.NewWriter(archiveFile)
	entry, err := archiveWriter.Create("bin/workflow.exe")
	if err == nil {
		_, err = entry.Write(executableBytes)
	}
	if closeErr := archiveWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := archiveFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := fixtureArtifacts()
	artifacts["workflow-windows-amd64.zip"] = archiveBytes
	manifest := validManifest(artifacts)
	manifest.PlatformSetup.SkillBundle.ManagedSkills = []string{"implement"}
	cliSum := sha256.Sum256(executableBytes)
	skillSum := sha256.Sum256([]byte("# implement\n"))
	manifest.BundledFiles = []BundledFile{{Path: "bin/workflow.exe", SHA256: hex.EncodeToString(cliSum[:])}, {Path: "skills/implement/SKILL.md", SHA256: hex.EncodeToString(skillSum[:])}}
	manifest.Provenance.Subjects = manifest.Artifacts
	raw, _, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
	policy, _ := json.Marshal(map[string]any{"schema_version": 1, "repository": manifest.Release.Repository, "workflow_path": manifest.Provenance.WorkflowPath, "key_id": manifest.Signature.KeyID, "signature_algorithm": manifest.Signature.Algorithm, "minimum_platform_version": "0.0.0", "public_key_file": filepath.Base(publicKeyPath)})
	write(policyPath, policy)
	scriptRoot := copyBootstrapSkillForTest(t, directory, policy, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	workflowHome := filepath.Join(directory, "workflow-home")
	hostFactsPath := filepath.Join(directory, "host-facts.json")
	planPath := filepath.Join(directory, "platform-plan.json")
	inspectCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(scriptRoot, "inspect-host.ps1"), "-Repository", directory, "-WorkflowHome", workflowHome)
	inspectOutput, err := inspectCommand.CombinedOutput()
	var inspected struct {
		SchemaVersion int  `json:"schema_version"`
		SupportedHost bool `json:"supported_host"`
		HostIdentity  struct {
			UserID              string `json:"user_id"`
			Username            string `json:"username"`
			WorkflowHomeOwnerID string `json:"workflow_home_owner_id"`
		} `json:"host_identity"`
	}
	if err != nil || json.Unmarshal(inspectOutput, &inspected) != nil || inspected.SchemaVersion != 1 || !inspected.SupportedHost || !strings.HasPrefix(inspected.HostIdentity.UserID, "S-") || inspected.HostIdentity.Username == "" || inspected.HostIdentity.WorkflowHomeOwnerID != inspected.HostIdentity.UserID {
		t.Fatalf("fresh host inspection on powershell.exe: %v (%s)", err, inspectOutput)
	}
	hostFacts, _ := json.Marshal(map[string]any{"schema_version": 1, "supported_host": true, "workflow_home": workflowHome, "host_identity": map[string]any{"user_id": "S-1-5-21-planner", "username": `DOMAIN\planner`, "workflow_home_owner_id": "S-1-5-21-planner"}, "workflow": map[string]any{"installed": false}, "docker": map[string]any{"installed": true, "desktop_version": manifest.PlatformSetup.Docker.Version, "engine_os": "linux", "engine_arch": "amd64"}, "github_credential": map[string]any{"exists": false, "path": filepath.Join(workflowHome, "state", "credentials", "github.pat")}, "codex_auth": map[string]any{"verified": true, "source": filepath.Join(directory, "codex-auth.json"), "fingerprint_sha256": strings.Repeat("9", 64)}, "codex_skills_root": filepath.Join(directory, "skills")})
	write(hostFactsPath, hostFacts)
	planCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(scriptRoot, "new-platform-bootstrap-plan.ps1"), "-ManifestPath", manifestPath, "-SignaturePath", signaturePath, "-HostFactsPath", hostFactsPath, "-OutputPath", planPath, "-GitHubOwner", "owner", "-GitHubOwnerType", "personal")
	planOutput, err := planCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh plan on powershell.exe: %v (%s)", err, planOutput)
	}
	var envelope struct {
		Digest        string `json:"digest_sha256"`
		CanonicalJSON string `json:"canonical_json"`
	}
	if err := json.Unmarshal(planOutput, &envelope); err != nil || envelope.Digest == "" {
		t.Fatalf("decode plan: %v (%s)", err, planOutput)
	}
	var approvedPlan struct {
		Preconditions []struct {
			Kind     string `json:"kind"`
			Expected string `json:"expected"`
		} `json:"preconditions"`
	}
	if err := json.Unmarshal([]byte(envelope.CanonicalJSON), &approvedPlan); err != nil {
		t.Fatal(err)
	}
	contractDigest := ""
	for _, precondition := range approvedPlan.Preconditions {
		if precondition.Kind == "platform_setup_contract" {
			contractDigest = precondition.Expected
		}
	}
	bundledRaw, err := json.Marshal(manifest.BundledFiles)
	if err != nil {
		t.Fatal(err)
	}
	bundledCanonical, bundledDigest, err := setupcontract.Canonicalize(bundledRaw)
	if err != nil || contractDigest == "" {
		t.Fatalf("derive fake post-apply platform readback: %v", err)
	}
	manifestSum := sha256.Sum256(raw)
	stdinCapture, argsCapture, callsCapture := filepath.Join(directory, "stdin.txt"), filepath.Join(directory, "args.txt"), filepath.Join(directory, "calls.txt")
	token := "ghp_fresh_bootstrap_must_not_leak"
	pinPath := filepath.Join(workflowHome, "config", "bootstrap-platform-release-pin.json")
	failedDownloadWrapper := `function Invoke-WebRequest { throw 'simulated archive download failure' }; & $env:WORKFLOW_TEST_INSTALLER -ManifestPath $env:WORKFLOW_TEST_MANIFEST -SignaturePath $env:WORKFLOW_TEST_SIGNATURE -PlanPath $env:WORKFLOW_TEST_PLAN -ApprovedDigest $env:WORKFLOW_TEST_DIGEST`
	failedDownload := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", failedDownloadWrapper)
	failedDownload.Env = append(os.Environ(), "WORKFLOW_TEST_INSTALLER="+filepath.Join(scriptRoot, "install-workflow-cli.ps1"), "WORKFLOW_TEST_MANIFEST="+manifestPath, "WORKFLOW_TEST_SIGNATURE="+signaturePath, "WORKFLOW_TEST_PLAN="+planPath, "WORKFLOW_TEST_DIGEST="+envelope.Digest)
	if failedOutput, failedErr := failedDownload.CombinedOutput(); failedErr == nil || !strings.Contains(string(failedOutput), "simulated archive download failure") {
		t.Fatalf("simulated failed install was not observed: err=%v output=%s", failedErr, failedOutput)
	}
	if _, err := os.Stat(pinPath); !os.IsNotExist(err) {
		t.Fatalf("failed download advanced the bootstrap release pin: %v", err)
	}
	wrapper := `function Invoke-WebRequest { param([string]$Uri,[string]$OutFile,[switch]$UseBasicParsing) Copy-Item -LiteralPath $env:WORKFLOW_TEST_ARCHIVE -Destination $OutFile }; & $env:WORKFLOW_TEST_INSTALLER -ManifestPath $env:WORKFLOW_TEST_MANIFEST -SignaturePath $env:WORKFLOW_TEST_SIGNATURE -PlanPath $env:WORKFLOW_TEST_PLAN -ApprovedDigest $env:WORKFLOW_TEST_DIGEST`
	installCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", wrapper)
	installCommand.Stdin = bytes.NewBufferString(token + "\n")
	installCommand.Env = append(os.Environ(), "WORKFLOW_TEST_ARCHIVE="+archivePath, "WORKFLOW_TEST_INSTALLER="+filepath.Join(scriptRoot, "install-workflow-cli.ps1"), "WORKFLOW_TEST_MANIFEST="+manifestPath, "WORKFLOW_TEST_SIGNATURE="+signaturePath, "WORKFLOW_TEST_PLAN="+planPath, "WORKFLOW_TEST_DIGEST="+envelope.Digest, "WORKFLOW_TEST_POLICY="+policyPath, "WORKFLOW_TEST_PUBLIC_KEY="+publicKeyPath, "WORKFLOW_TEST_STDIN="+stdinCapture, "WORKFLOW_TEST_ARGS="+argsCapture, "WORKFLOW_TEST_CALLS="+callsCapture, "WORKFLOW_TEST_CP_DIGEST="+envelope.Digest, "WORKFLOW_TEST_RELEASE_VERSION="+manifest.Release.Version, "WORKFLOW_TEST_MANIFEST_DIGEST="+hex.EncodeToString(manifestSum[:]), "WORKFLOW_TEST_CONTRACT_DIGEST="+contractDigest, "WORKFLOW_TEST_CLI_DIGEST="+hex.EncodeToString(cliSum[:]), "WORKFLOW_TEST_BUNDLE_JSON="+string(bundledCanonical), "WORKFLOW_TEST_BUNDLE_DIGEST="+bundledDigest)
	installOutput, err := installCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh install on powershell.exe: %v (%s)", err, installOutput)
	}
	if strings.Contains(string(installOutput), token) {
		t.Fatalf("installer leaked PAT in ordinary output: %s", installOutput)
	}
	captured, err := os.ReadFile(stdinCapture)
	if err != nil || strings.TrimSpace(string(captured)) != token {
		t.Fatalf("workflow setup apply stdin=%q err=%v", captured, err)
	}
	args, err := os.ReadFile(argsCapture)
	if err != nil || strings.Contains(string(args), token) || !strings.Contains(string(args), "setup\napply\n--plan") || !strings.Contains(string(args), "--approved-digest\n"+envelope.Digest) {
		t.Fatalf("workflow setup apply args=%q err=%v", args, err)
	}
	calls, err := os.ReadFile(callsCapture)
	if err != nil || !strings.Contains(string(calls), "setup inspect-platform --workflow-home ") || !strings.Contains(string(calls), "status --workflow-home ") {
		t.Fatalf("installer did not verify durable and live Control Plane authorization after apply: calls=%q err=%v", calls, err)
	}
	var cliRepairPlan setupcontract.Plan
	if err := json.Unmarshal([]byte(envelope.CanonicalJSON), &cliRepairPlan); err != nil {
		t.Fatal(err)
	}
	approvedEffects := append([]setupcontract.Effect(nil), cliRepairPlan.Effects...)
	cliRepairPlan.Effects = nil
	for _, effect := range approvedEffects {
		if effect.Kind == "platform_cli" {
			cliRepairPlan.Effects = append(cliRepairPlan.Effects, effect)
		}
	}
	cliRepairRaw, err := json.Marshal(cliRepairPlan)
	if err != nil {
		t.Fatal(err)
	}
	_, cliRepairCanonical, cliRepairDigest, err := setupcontract.ParsePlan(cliRepairRaw)
	if err != nil {
		t.Fatal(err)
	}
	cliRepairEnvelope, _ := json.Marshal(map[string]any{"status": "plan_required", "digest_sha256": cliRepairDigest, "canonical_json": string(cliRepairCanonical), "plan": cliRepairPlan, "projection": "CLI-only repair"})
	cliRepairPlanPath := filepath.Join(directory, "cli-repair-plan.json")
	write(cliRepairPlanPath, cliRepairEnvelope)
	priorControlPlaneDigest := strings.Repeat("7", 64)
	cliRepairCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", wrapper)
	cliRepairCommand.Env = replaceTestEnvironment(installCommand.Env, map[string]string{"WORKFLOW_TEST_PLAN": cliRepairPlanPath, "WORKFLOW_TEST_DIGEST": cliRepairDigest, "WORKFLOW_TEST_CP_DIGEST": priorControlPlaneDigest})
	if output, err := cliRepairCommand.CombinedOutput(); err != nil {
		t.Fatalf("CLI-only repair on powershell.exe: %v (%s)", err, output)
	}
	cliRepairPin, err := os.ReadFile(pinPath)
	var cliRepairPinDocument map[string]any
	if err != nil || json.Unmarshal(cliRepairPin, &cliRepairPinDocument) != nil || cliRepairPinDocument["control_plane_plan_digest_sha256"] != priorControlPlaneDigest || cliRepairPinDocument["control_plane_plan_digest_sha256"] == cliRepairDigest {
		t.Fatalf("CLI-only repair did not preserve the exact post-apply verified prior Control Plane authorization: pin=%s err=%v", cliRepairPin, err)
	}
	if _, err := os.Stat(pinPath); err != nil {
		t.Fatalf("bootstrap did not durably pin the approved release: %v", err)
	}
	inspectWrapper := `function Get-Acl { param([string]$LiteralPath) [pscustomobject]@{ Owner = [Security.Principal.WindowsIdentity]::GetCurrent().Name } }; & $env:WORKFLOW_TEST_INSPECT -Repository $env:WORKFLOW_TEST_REPOSITORY -WorkflowHome $env:WORKFLOW_TEST_HOME`
	inspectPinned := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", inspectWrapper)
	inspectPinned.Env = append(os.Environ(), "WORKFLOW_TEST_INSPECT="+filepath.Join(scriptRoot, "inspect-host.ps1"), "WORKFLOW_TEST_REPOSITORY="+directory, "WORKFLOW_TEST_HOME="+workflowHome)
	pinnedOutput, err := inspectPinned.CombinedOutput()
	var pinnedFacts struct {
		Workflow struct {
			Installed bool `json:"installed"`
		} `json:"workflow"`
		Platform struct {
			InstallationRecorded  bool   `json:"installation_recorded"`
			Version               string `json:"version"`
			ReleaseManifestDigest string `json:"release_manifest_digest"`
		} `json:"platform"`
	}
	manifestDigest := sha256.Sum256(raw)
	if decodeErr := json.Unmarshal(pinnedOutput, &pinnedFacts); err != nil || decodeErr != nil || pinnedFacts.Workflow.Installed || !pinnedFacts.Platform.InstallationRecorded || pinnedFacts.Platform.Version != manifest.Release.Version || pinnedFacts.Platform.ReleaseManifestDigest != hex.EncodeToString(manifestDigest[:]) {
		t.Fatalf("inspect did not preserve the exact release pin without workflow.exe: err=%v decode=%v output=%s", err, decodeErr, pinnedOutput)
	}
	// A valid pin is authority to inspect only the exact pinned executable. A
	// different, otherwise runnable workflow.exe must not execute even once.
	mismatchedSource := filepath.Join(directory, "mismatched-workflow.go")
	mismatchedExecutable := filepath.Join(directory, "mismatched-workflow.exe")
	mismatchedProgram := `package main
import ("os"; "strings")
func main() { _ = os.WriteFile(os.Getenv("WORKFLOW_TEST_SIDE_EFFECT"), []byte("executed:"+strings.Join(os.Args[1:], " ")), 0600) }
`
	write(mismatchedSource, []byte(mismatchedProgram))
	if output, buildErr := exec.Command("go", "build", "-o", mismatchedExecutable, mismatchedSource).CombinedOutput(); buildErr != nil {
		t.Fatalf("build mismatched workflow.exe: %v (%s)", buildErr, output)
	}
	installedExecutable := filepath.Join(workflowHome, "bin", "workflow.exe")
	if err := os.MkdirAll(filepath.Dir(installedExecutable), 0o700); err != nil {
		t.Fatal(err)
	}
	mismatchedBytes, err := os.ReadFile(mismatchedExecutable)
	if err != nil {
		t.Fatal(err)
	}
	write(installedExecutable, mismatchedBytes)
	sideEffectPath := filepath.Join(directory, "untrusted-workflow-executed.txt")
	mismatchedInspect := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", inspectWrapper)
	mismatchedInspect.Env = append(os.Environ(), "WORKFLOW_TEST_INSPECT="+filepath.Join(scriptRoot, "inspect-host.ps1"), "WORKFLOW_TEST_REPOSITORY="+directory, "WORKFLOW_TEST_HOME="+workflowHome, "WORKFLOW_TEST_SIDE_EFFECT="+sideEffectPath)
	mismatchedOutput, mismatchedErr := mismatchedInspect.CombinedOutput()
	var mismatchedFacts struct {
		Workflow struct {
			Installed  bool   `json:"installed"`
			TrustState string `json:"trust_state"`
			Diagnostic string `json:"diagnostic"`
		} `json:"workflow"`
	}
	if decodeErr := json.Unmarshal(mismatchedOutput, &mismatchedFacts); mismatchedErr != nil || decodeErr != nil || !mismatchedFacts.Workflow.Installed || mismatchedFacts.Workflow.TrustState != "repair_required" || !strings.Contains(mismatchedFacts.Workflow.Diagnostic, "SHA-256") {
		t.Fatalf("mismatched installed CLI was not reported as a non-executed repair: err=%v decode=%v output=%s", mismatchedErr, decodeErr, mismatchedOutput)
	}
	if _, err := os.Stat(sideEffectPath); !os.IsNotExist(err) {
		t.Fatalf("inspect-host executed a workflow.exe before validating its pinned SHA-256: %v", err)
	}
	pin, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatal(err)
	}
	// Even the exact pinned bytes remain non-executable until the fixed path is
	// proved to be owned by the current Windows user.
	write(installedExecutable, executableBytes)
	ownerSideEffectStdin := filepath.Join(directory, "owner-mismatch-stdin.txt")
	ownerSideEffectArgs := filepath.Join(directory, "owner-mismatch-args.txt")
	ownerMismatchWrapper := `function Get-Acl { param([string]$LiteralPath) if ([IO.Path]::GetExtension($LiteralPath) -ieq '.exe') { [pscustomobject]@{ Owner = 'BUILTIN\Administrators' } } else { [pscustomobject]@{ Owner = [Security.Principal.WindowsIdentity]::GetCurrent().Name } } }; & $env:WORKFLOW_TEST_INSPECT -Repository $env:WORKFLOW_TEST_REPOSITORY -WorkflowHome $env:WORKFLOW_TEST_HOME`
	ownerMismatchInspect := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ownerMismatchWrapper)
	ownerMismatchInspect.Env = append(os.Environ(), "WORKFLOW_TEST_INSPECT="+filepath.Join(scriptRoot, "inspect-host.ps1"), "WORKFLOW_TEST_REPOSITORY="+directory, "WORKFLOW_TEST_HOME="+workflowHome, "WORKFLOW_TEST_STDIN="+ownerSideEffectStdin, "WORKFLOW_TEST_ARGS="+ownerSideEffectArgs)
	ownerMismatchOutput, ownerMismatchErr := ownerMismatchInspect.CombinedOutput()
	var ownerMismatchFacts struct {
		Workflow struct {
			TrustState string `json:"trust_state"`
			Diagnostic string `json:"diagnostic"`
		} `json:"workflow"`
	}
	if decodeErr := json.Unmarshal(ownerMismatchOutput, &ownerMismatchFacts); ownerMismatchErr != nil || decodeErr != nil || ownerMismatchFacts.Workflow.TrustState != "conflict" || !strings.Contains(ownerMismatchFacts.Workflow.Diagnostic, "owned by the current Windows user") {
		t.Fatalf("non-current-user CLI ownership was not reported as a non-executed conflict: err=%v decode=%v output=%s", ownerMismatchErr, decodeErr, ownerMismatchOutput)
	}
	for _, path := range []string{ownerSideEffectStdin, ownerSideEffectArgs} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("inspect-host executed workflow.exe before validating current-user ownership (%s): %v", path, err)
		}
	}
	if err := os.Remove(pinPath); err != nil {
		t.Fatal(err)
	}
	missingPinInspect := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", inspectWrapper)
	missingPinInspect.Env = append(os.Environ(), "WORKFLOW_TEST_INSPECT="+filepath.Join(scriptRoot, "inspect-host.ps1"), "WORKFLOW_TEST_REPOSITORY="+directory, "WORKFLOW_TEST_HOME="+workflowHome, "WORKFLOW_TEST_STDIN="+ownerSideEffectStdin, "WORKFLOW_TEST_ARGS="+ownerSideEffectArgs)
	missingPinOutput, missingPinErr := missingPinInspect.CombinedOutput()
	var missingPinFacts struct {
		Workflow struct {
			TrustState string `json:"trust_state"`
			Diagnostic string `json:"diagnostic"`
		} `json:"workflow"`
	}
	if decodeErr := json.Unmarshal(missingPinOutput, &missingPinFacts); missingPinErr != nil || decodeErr != nil || missingPinFacts.Workflow.TrustState != "repair_required" || !strings.Contains(missingPinFacts.Workflow.Diagnostic, "pin is missing") {
		t.Fatalf("missing bootstrap pin was not reported as a non-executed repair: err=%v decode=%v output=%s", missingPinErr, decodeErr, missingPinOutput)
	}
	for _, path := range []string{ownerSideEffectStdin, ownerSideEffectArgs} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("inspect-host executed workflow.exe without a bootstrap pin (%s): %v", path, err)
		}
	}
	var pinDocument map[string]any
	if json.Unmarshal(pin, &pinDocument) != nil {
		t.Fatal("bootstrap release pin is not JSON")
	}
	if pinDocument["release_bundled_files_json"] == "" || pinDocument["release_bundled_files_digest_sha256"] == "" || pinDocument["control_plane_plan_digest_sha256"] != priorControlPlaneDigest {
		t.Fatalf("bootstrap release pin omitted signed bundle inventory or Control Plane authorization fence: %#v", pinDocument)
	}
	pinDocument["unexpected_future_authority"] = "not-approved"
	tamperedPin, _ := json.Marshal(pinDocument)
	if err := os.WriteFile(pinPath, tamperedPin, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperedInspect := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", inspectWrapper)
	tamperedInspect.Env = append(os.Environ(), "WORKFLOW_TEST_INSPECT="+filepath.Join(scriptRoot, "inspect-host.ps1"), "WORKFLOW_TEST_REPOSITORY="+directory, "WORKFLOW_TEST_HOME="+workflowHome, "WORKFLOW_TEST_SIDE_EFFECT="+sideEffectPath)
	tamperedOutput, tamperedErr := tamperedInspect.CombinedOutput()
	var tamperedFacts struct {
		Workflow struct {
			TrustState string `json:"trust_state"`
			Diagnostic string `json:"diagnostic"`
		} `json:"workflow"`
	}
	if decodeErr := json.Unmarshal(tamperedOutput, &tamperedFacts); tamperedErr != nil || decodeErr != nil || tamperedFacts.Workflow.TrustState != "conflict" || !strings.Contains(tamperedFacts.Workflow.Diagnostic, "missing or unknown fields") {
		t.Fatalf("tampered bootstrap pin was not reported as a non-executed conflict: err=%v decode=%v output=%s", tamperedErr, decodeErr, tamperedOutput)
	}
	if _, err := os.Stat(sideEffectPath); !os.IsNotExist(err) {
		t.Fatalf("inspect-host executed workflow.exe after a bootstrap pin conflict: %v", err)
	}
}

func replaceTestEnvironment(environment []string, replacements map[string]string) []string {
	result := append([]string(nil), environment...)
	for key, value := range replacements {
		prefix := key + "="
		replaced := false
		for index, entry := range result {
			if strings.HasPrefix(entry, prefix) {
				result[index] = prefix + value
				replaced = true
			}
		}
		if !replaced {
			result = append(result, prefix+value)
		}
	}
	return result
}

func TestFreshHostInspectionResolvesCodexDoctorAuthBeforeWorkflowCLIExistsOnPowerShell51(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is the supported bootstrap shell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	directory := t.TempDir()
	authPath := filepath.Join(directory, "codex-home", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	auth := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account","id_token":"id","refresh_token":"refresh"}}`)
	if err := os.WriteFile(authPath, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSource := filepath.Join(directory, "fake-codex.go")
	fakeCodex := filepath.Join(directory, "codex.exe")
	program := `package main
import ("encoding/json"; "fmt"; "os")
func main() {
 if len(os.Args)>2 && os.Args[1]=="doctor" && os.Args[2]=="--json" { report:=map[string]any{"schemaVersion":1,"codexVersion":"0.147.0","checks":map[string]any{"auth.credentials":map[string]any{"status":"ok","details":map[string]string{"auth file":os.Getenv("WORKFLOW_TEST_CODEX_AUTH"),"stored ChatGPT tokens":"true","stored auth mode":"chatgpt"}},"config.load":map[string]any{"status":"ok","details":map[string]string{"CODEX_HOME":os.Getenv("WORKFLOW_TEST_CODEX_HOME")}}}}; _=json.NewEncoder(os.Stdout).Encode(report); os.Exit(1) }
 if len(os.Args)>2 && os.Args[1]=="login" && os.Args[2]=="status" { fmt.Println("Logged in using ChatGPT"); return }
 fmt.Println("codex-cli 0.147.0")
}`
	if err := os.WriteFile(fakeSource, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", fakeCodex, fakeSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake codex.exe: %v (%s)", err, output)
	}
	_, currentFile, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "skills", "setup-agent-workflow", "scripts", "inspect-host.ps1"))
	command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Repository", directory, "-WorkflowHome", filepath.Join(directory, "workflow-home"))
	command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "WORKFLOW_TEST_CODEX_AUTH="+authPath, "WORKFLOW_TEST_CODEX_HOME="+filepath.Dir(authPath))
	output, err := command.CombinedOutput()
	var facts struct {
		CodexAuth struct {
			Verified          bool   `json:"verified"`
			Source            string `json:"source"`
			FingerprintSHA256 string `json:"fingerprint_sha256"`
		} `json:"codex_auth"`
	}
	if decodeErr := json.Unmarshal(bytes.TrimPrefix(output, []byte{0xef, 0xbb, 0xbf}), &facts); err != nil || decodeErr != nil {
		t.Fatalf("fresh inspect output=%q run=%v decode=%v", output, err, decodeErr)
	}
	sum := sha256.Sum256(auth)
	if !facts.CodexAuth.Verified || facts.CodexAuth.Source != authPath || facts.CodexAuth.FingerprintSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("fresh Codex auth facts=%#v", facts.CodexAuth)
	}
}

func TestBootstrapInstallerUsesOnlyExactPinnedArchiveExecutable(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	scriptPath := filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts", "install-workflow-cli.ps1")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, required := range []string{`Join-Path $expanded "bin\workflow.exe"`, "Workflow CLI archive must contain only exact bin/workflow.exe", "Workflow CLI executable checksum differs from the signed workflow_cli_sha256"} {
		if !strings.Contains(content, required) {
			t.Fatalf("bootstrap installer lacks exact archive executable contract %q", required)
		}
	}
	if strings.Contains(content, `-Filter workflow.exe -Recurse`) {
		t.Fatal("bootstrap installer still chooses a recursive or ambiguous workflow.exe")
	}
}

func TestHostInspectionRejectsExistingWorkflowHomeOwnedByAnotherSIDOnPowerShell51(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is the supported bootstrap shell")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	directory := t.TempDir()
	workflowHome := filepath.Join(directory, "workflow-home")
	if err := os.MkdirAll(workflowHome, 0o700); err != nil {
		t.Fatal(err)
	}
	_, currentFile, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "skills", "setup-agent-workflow", "scripts", "inspect-host.ps1"))
	wrapper := `function Get-Acl { param([string]$LiteralPath) [pscustomobject]@{ Owner = $env:WORKFLOW_TEST_OWNER } }; & $env:WORKFLOW_TEST_INSPECT -Repository $env:WORKFLOW_TEST_REPOSITORY -WorkflowHome $env:WORKFLOW_TEST_HOME`
	for _, owner := range []string{`BUILTIN\Administrators`, `NT AUTHORITY\SYSTEM`} {
		t.Run(owner, func(t *testing.T) {
			command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", wrapper)
			command.Env = append(os.Environ(), "WORKFLOW_TEST_INSPECT="+script, "WORKFLOW_TEST_REPOSITORY="+directory, "WORKFLOW_TEST_HOME="+workflowHome, "WORKFLOW_TEST_OWNER="+owner)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "must be owned by the current Windows user") {
				t.Fatalf("inspection accepted %s-owned Workflow Home: err=%v output=%s", owner, err, output)
			}
		})
	}
}
