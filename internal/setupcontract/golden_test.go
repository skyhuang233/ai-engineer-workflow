package setupcontract

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSchemaV1GoldenPlanMatchesGoAndPowerShell(t *testing.T) {
	inputPath := filepath.Join("testdata", "schema-v1-plan.input.json")
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedCanonical, err := os.ReadFile(filepath.Join("testdata", "schema-v1-plan.canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectedCanonical = bytes.TrimSuffix(expectedCanonical, []byte("\n"))
	expectedCanonical = bytes.TrimSuffix(expectedCanonical, []byte("\r"))
	expectedDigest, err := os.ReadFile(filepath.Join("testdata", "schema-v1-plan.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	_, canonical, digest, err := ParsePlan(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, expectedCanonical) || digest != strings.TrimSpace(string(expectedDigest)) {
		t.Fatalf("Go golden mismatch: digest %s, JSON %s", digest, canonical)
	}
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell parity runs on the supported bootstrap host")
	}
	powershellOutput := filepath.Join(t.TempDir(), "canonical.json")
	command := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join("testdata", "Get-SetupCanonicalDigest.ps1"), "-Path", inputPath, "-OutputPath", powershellOutput)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell canonicalizer: %v: %s", err, output)
	}
	powershellCanonical, err := os.ReadFile(powershellOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(powershellCanonical, expectedCanonical) || strings.TrimSpace(string(output)) != strings.TrimSpace(string(expectedDigest)) {
		t.Fatalf("PowerShell golden mismatch: digest %s, JSON %s", output, powershellCanonical)
	}
}
