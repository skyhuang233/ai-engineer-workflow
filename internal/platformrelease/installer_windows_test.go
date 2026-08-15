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
import ("io"; "os"; "strings")
func main() { input, _ := io.ReadAll(os.Stdin); _ = os.WriteFile(os.Getenv("WORKFLOW_TEST_STDIN"), input, 0600); _ = os.WriteFile(os.Getenv("WORKFLOW_TEST_ARGS"), []byte(strings.Join(os.Args[1:], "\n")), 0600) }
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
	_, currentFile, _, _ := runtime.Caller(0)
	scriptRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "skills", "setup-agent-workflow", "scripts"))
	workflowHome := filepath.Join(directory, "workflow-home")
	hostFactsPath := filepath.Join(directory, "host-facts.json")
	planPath := filepath.Join(directory, "platform-plan.json")
	inspectCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(scriptRoot, "inspect-host.ps1"), "-Repository", directory, "-WorkflowHome", workflowHome)
	inspectOutput, err := inspectCommand.CombinedOutput()
	var inspected struct {
		SchemaVersion int  `json:"schema_version"`
		SupportedHost bool `json:"supported_host"`
	}
	if err != nil || json.Unmarshal(inspectOutput, &inspected) != nil || inspected.SchemaVersion != 1 || !inspected.SupportedHost {
		t.Fatalf("fresh host inspection on powershell.exe: %v (%s)", err, inspectOutput)
	}
	hostFacts, _ := json.Marshal(map[string]any{"schema_version": 1, "supported_host": true, "workflow_home": workflowHome, "workflow": map[string]any{"installed": false}, "docker": map[string]any{"installed": true, "desktop_version": manifest.PlatformSetup.Docker.Version, "engine_os": "linux", "engine_arch": "amd64"}, "github_credential": map[string]any{"exists": false, "path": filepath.Join(workflowHome, "state", "credentials", "github.pat")}, "codex_skills_root": filepath.Join(directory, "skills")})
	write(hostFactsPath, hostFacts)
	planCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(scriptRoot, "new-platform-bootstrap-plan.ps1"), "-ManifestPath", manifestPath, "-SignaturePath", signaturePath, "-HostFactsPath", hostFactsPath, "-OutputPath", planPath, "-GitHubOwner", "owner", "-PolicyPath", policyPath, "-PublicKeyPath", publicKeyPath)
	planOutput, err := planCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh plan on powershell.exe: %v (%s)", err, planOutput)
	}
	var envelope struct {
		Digest string `json:"digest_sha256"`
	}
	if err := json.Unmarshal(planOutput, &envelope); err != nil || envelope.Digest == "" {
		t.Fatalf("decode plan: %v (%s)", err, planOutput)
	}
	stdinCapture, argsCapture := filepath.Join(directory, "stdin.txt"), filepath.Join(directory, "args.txt")
	token := "ghp_fresh_bootstrap_must_not_leak"
	wrapper := `function Invoke-WebRequest { param([string]$Uri,[string]$OutFile,[switch]$UseBasicParsing) Copy-Item -LiteralPath $env:WORKFLOW_TEST_ARCHIVE -Destination $OutFile }; & $env:WORKFLOW_TEST_INSTALLER -ManifestPath $env:WORKFLOW_TEST_MANIFEST -SignaturePath $env:WORKFLOW_TEST_SIGNATURE -PlanPath $env:WORKFLOW_TEST_PLAN -ApprovedDigest $env:WORKFLOW_TEST_DIGEST -PolicyPath $env:WORKFLOW_TEST_POLICY -PublicKeyPath $env:WORKFLOW_TEST_PUBLIC_KEY`
	installCommand := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", wrapper)
	installCommand.Stdin = bytes.NewBufferString(token + "\n")
	installCommand.Env = append(os.Environ(), "WORKFLOW_TEST_ARCHIVE="+archivePath, "WORKFLOW_TEST_INSTALLER="+filepath.Join(scriptRoot, "install-workflow-cli.ps1"), "WORKFLOW_TEST_MANIFEST="+manifestPath, "WORKFLOW_TEST_SIGNATURE="+signaturePath, "WORKFLOW_TEST_PLAN="+planPath, "WORKFLOW_TEST_DIGEST="+envelope.Digest, "WORKFLOW_TEST_POLICY="+policyPath, "WORKFLOW_TEST_PUBLIC_KEY="+publicKeyPath, "WORKFLOW_TEST_STDIN="+stdinCapture, "WORKFLOW_TEST_ARGS="+argsCapture)
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
}
