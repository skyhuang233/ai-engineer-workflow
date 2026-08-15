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
)

func TestResolvePlatformReleaseDownloadsAndVerifiesLatestStableForFreshInstall(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixture(t, "1.2.3")
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))

	output, runErr := fixture.run(t, powershell, factsPath, "", false)
	if runErr != nil {
		t.Fatalf("resolve latest stable: %v\n%s", runErr, output)
	}
	var result struct {
		Verified       bool   `json:"verified"`
		ReleaseVersion string `json:"release_version"`
		ManifestPath   string `json:"manifest_path"`
		SignaturePath  string `json:"signature_path"`
		TempDirectory  string `json:"temp_directory"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode resolver output: %v\n%s", err, output)
	}
	if !result.Verified || result.ReleaseVersion != "1.2.3" || filepath.Base(result.ManifestPath) != "platform-release.json" || filepath.Base(result.SignaturePath) != "platform-release.json.sig" || filepath.Dir(result.ManifestPath) != result.TempDirectory || filepath.Dir(result.SignaturePath) != result.TempDirectory {
		t.Fatalf("unexpected resolver result: %#v", result)
	}
	if _, err := os.Stat(result.ManifestPath); err != nil {
		t.Fatalf("verified manifest was not retained in task temp: %v", err)
	}
	requests, err := os.ReadFile(fixture.requestLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://api.github.com/repos/owner/platform/releases/latest",
		"https://github.com/owner/platform/releases/download/platform-v1.2.3/platform-release.json",
		"https://github.com/owner/platform/releases/download/platform-v1.2.3/platform-release.json.sig",
	}
	for _, request := range want {
		if !strings.Contains(string(requests), request) {
			t.Fatalf("resolver did not use fixed public GitHub endpoint %q; requests=%q", request, requests)
		}
	}
	if strings.Contains(string(requests), "evil.example") {
		t.Fatalf("resolver trusted release asset URL: %q", requests)
	}
}

func TestResolvePlatformReleaseEnforcesDurableVersionTransitions(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	tests := []struct {
		name          string
		candidate     string
		requested     string
		allowUpgrade  bool
		durableDigest string
		wantSuccess   bool
		wantSelection string
		wantError     string
	}{
		{name: "implicit exact repair", candidate: "1.2.0", wantSuccess: true, wantSelection: "durable-repair"},
		{name: "explicit exact repair", candidate: "1.2.0", requested: "1.2.0", wantSuccess: true, wantSelection: "durable-repair"},
		{name: "explicit upgrade", candidate: "1.3.0", requested: "1.3.0", allowUpgrade: true, wantSuccess: true, wantSelection: "explicit-upgrade"},
		{name: "greater without authorization", candidate: "1.3.0", requested: "1.3.0", wantError: "requires explicit AllowUpgrade"},
		{name: "allow upgrade on same version", candidate: "1.2.0", requested: "1.2.0", allowUpgrade: true, wantError: "only a version greater"},
		{name: "downgrade", candidate: "1.1.9", requested: "1.1.9", allowUpgrade: true, wantError: "older than the durable"},
		{name: "same version wrong durable manifest", candidate: "1.2.0", durableDigest: strings.Repeat("0", 64), wantError: "exact signed manifest digest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResolverFixture(t, test.candidate)
			raw, err := os.ReadFile(fixture.manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			durableDigest := test.durableDigest
			if durableDigest == "" {
				durableDigest = resolverDigest(raw)
			}
			factsPath := filepath.Join(fixture.directory, "host-facts.json")
			facts, _ := json.Marshal(map[string]any{"schema_version": 1, "platform": map[string]any{"installation_recorded": true, "version": "1.2.0", "release_manifest_digest": durableDigest}})
			writeResolverFile(t, factsPath, facts)
			output, runErr := fixture.run(t, powershell, factsPath, test.requested, test.allowUpgrade)
			if test.wantSuccess {
				if runErr != nil {
					t.Fatalf("resolve transition: %v\n%s", runErr, output)
				}
				var result struct {
					Selection string `json:"selection"`
				}
				if err := json.Unmarshal([]byte(output), &result); err != nil || result.Selection != test.wantSelection {
					t.Fatalf("selection=%q err=%v output=%s", result.Selection, err, output)
				}
				return
			}
			if runErr == nil || !strings.Contains(output, test.wantError) || strings.Contains(output, `"manifest_path"`) {
				t.Fatalf("transition was not rejected before emitting paths: err=%v output=%s", runErr, output)
			}
		})
	}
}

func TestResolvePlatformReleaseFailsClosedBeforeOutputForMissingKeyOrInvalidContract(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	t.Run("missing pinned key", func(t *testing.T) {
		fixture := newResolverFixture(t, "1.2.3")
		factsPath := filepath.Join(fixture.directory, "host-facts.json")
		writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
		if err := os.Remove(fixture.publicKeyPath); err != nil {
			t.Fatal(err)
		}
		output, runErr := fixture.run(t, powershell, factsPath, "", false)
		if runErr == nil || !strings.Contains(output, "Pinned Platform Release public key is missing") || strings.Contains(output, `"manifest_path"`) {
			t.Fatalf("missing key did not fail closed: err=%v output=%s", runErr, output)
		}
		if requests, err := os.ReadFile(fixture.requestLog); err == nil && len(requests) != 0 {
			t.Fatalf("resolver used the network before validating its key: %q", requests)
		}
	})
	t.Run("signed invalid platform contract", func(t *testing.T) {
		fixture := newResolverFixtureWithMutation(t, "1.2.3", func(manifest *Manifest) {
			manifest.PlatformSetup.WorkflowHomeDefault = `%TEMP%\UntrustedWorkflow`
		})
		factsPath := filepath.Join(fixture.directory, "host-facts.json")
		writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
		output, runErr := fixture.run(t, powershell, factsPath, "", false)
		if runErr == nil || !strings.Contains(output, "Workflow Home default") || strings.Contains(output, `"manifest_path"`) {
			t.Fatalf("invalid signed contract emitted trusted paths: err=%v output=%s", runErr, output)
		}
	})
}

type resolverFixture struct {
	directory     string
	scriptPath    string
	policyPath    string
	publicKeyPath string
	manifestPath  string
	signaturePath string
	metadataPath  string
	requestLog    string
}

func newResolverFixture(t *testing.T, version string) resolverFixture {
	return newResolverFixtureWithMutation(t, version, nil)
}

func newResolverFixtureWithMutation(t *testing.T, version string, mutate func(*Manifest)) resolverFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validManifest(fixtureArtifacts())
	manifest.Release.Repository = "owner/platform"
	manifest.Provenance.Repository = "owner/platform"
	manifest.Release.Version = version
	manifest.Release.Tag = "platform-v" + version
	if mutate != nil {
		mutate(&manifest)
	}
	raw, _, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	fixture := resolverFixture{
		directory:    directory,
		manifestPath: filepath.Join(directory, "source-platform-release.json"), signaturePath: filepath.Join(directory, "source-platform-release.json.sig"),
		policyPath: filepath.Join(directory, "setup-agent-workflow", "trust", "release-policy.json"), publicKeyPath: filepath.Join(directory, "setup-agent-workflow", "trust", "platform-release-public-key.pem"),
		metadataPath: filepath.Join(directory, "release.json"), requestLog: filepath.Join(directory, "requests.log"),
	}
	_, current, _, _ := runtime.Caller(0)
	sourceScripts := filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", "skills", "setup-agent-workflow", "scripts"))
	fixture.scriptPath = filepath.Join(directory, "setup-agent-workflow", "scripts", "resolve-platform-release.ps1")
	if err := os.MkdirAll(filepath.Dir(fixture.scriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(fixture.policyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, scriptName := range []string{"resolve-platform-release.ps1", "verify-platform-release.ps1"} {
		source, err := os.ReadFile(filepath.Join(sourceScripts, scriptName))
		if err != nil {
			t.Fatal(err)
		}
		writeResolverFile(t, filepath.Join(filepath.Dir(fixture.scriptPath), scriptName), source)
	}
	writeResolverFile(t, fixture.manifestPath, raw)
	writeResolverFile(t, fixture.signaturePath, signature)
	writeResolverFile(t, fixture.publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	policy, _ := json.Marshal(map[string]any{"schema_version": 1, "repository": "owner/platform", "workflow_path": manifest.Provenance.WorkflowPath, "key_id": manifest.Signature.KeyID, "signature_algorithm": manifest.Signature.Algorithm, "minimum_platform_version": "0.0.0", "public_key_file": filepath.Base(fixture.publicKeyPath)})
	writeResolverFile(t, fixture.policyPath, policy)
	metadata, _ := json.Marshal(map[string]any{"tag_name": manifest.Release.Tag, "draft": false, "prerelease": false, "assets": []map[string]string{{"name": "platform-release.json", "browser_download_url": "https://evil.example/manifest"}, {"name": "platform-release.json.sig", "browser_download_url": "https://evil.example/signature"}}})
	writeResolverFile(t, fixture.metadataPath, metadata)
	return fixture
}

func (fixture resolverFixture) run(t *testing.T, powershell, factsPath, version string, allowUpgrade bool) (string, error) {
	t.Helper()
	wrapper := filepath.Join(fixture.directory, "invoke-resolver.ps1")
	writeResolverFile(t, wrapper, []byte(`
function Invoke-WebRequest {
    param([string]$Uri, $Headers, [string]$OutFile, [switch]$UseBasicParsing)
    Add-Content -LiteralPath $env:WORKFLOW_TEST_REQUEST_LOG -Value $Uri
    if ([string]::IsNullOrWhiteSpace($OutFile)) {
        return [pscustomobject]@{ Content = [IO.File]::ReadAllText($env:WORKFLOW_TEST_METADATA) }
    }
    if ($Uri.EndsWith('/platform-release.json.sig')) { Copy-Item -LiteralPath $env:WORKFLOW_TEST_SIGNATURE -Destination $OutFile }
    elseif ($Uri.EndsWith('/platform-release.json')) { Copy-Item -LiteralPath $env:WORKFLOW_TEST_MANIFEST -Destination $OutFile }
    else { throw "unexpected download $Uri" }
}
$parameters = @{
    HostFactsPath = $env:WORKFLOW_TEST_FACTS
}
if (-not [string]::IsNullOrWhiteSpace($env:WORKFLOW_TEST_VERSION)) { $parameters.Version = $env:WORKFLOW_TEST_VERSION }
if ($env:WORKFLOW_TEST_ALLOW_UPGRADE -eq '1') { $parameters.AllowUpgrade = $true }
& $env:WORKFLOW_TEST_RESOLVER @parameters
`))
	command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", wrapper)
	allow := "0"
	if allowUpgrade {
		allow = "1"
	}
	command.Env = append(os.Environ(),
		"WORKFLOW_TEST_REQUEST_LOG="+fixture.requestLog,
		"WORKFLOW_TEST_METADATA="+fixture.metadataPath,
		"WORKFLOW_TEST_MANIFEST="+fixture.manifestPath,
		"WORKFLOW_TEST_SIGNATURE="+fixture.signaturePath,
		"WORKFLOW_TEST_FACTS="+factsPath,
		"WORKFLOW_TEST_VERSION="+version,
		"WORKFLOW_TEST_ALLOW_UPGRADE="+allow,
		"WORKFLOW_TEST_RESOLVER="+fixture.scriptPath,
	)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func writeResolverFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolverDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
