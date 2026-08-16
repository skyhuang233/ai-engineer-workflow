package platformrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		TempDirectory  string `json:"temp_directory"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode resolver output: %v\n%s", err, output)
	}
	if !result.Verified || result.ReleaseVersion != "1.2.3" || filepath.Base(result.ManifestPath) != "platform-release.json" || filepath.Dir(result.ManifestPath) != result.TempDirectory {
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
		"https://api.github.com/repos/owner/platform/releases?per_page=100&page=1",
		"https://api.github.com/repos/owner/platform/releases?per_page=100&page=2",
		"https://api.github.com/repos/owner/platform/releases/assets/2",
	}
	for _, request := range want {
		if !strings.Contains(string(requests), request) {
			t.Fatalf("resolver did not use fixed public GitHub endpoint %q; requests=%q", request, requests)
		}
	}
	if strings.Contains(string(requests), "github.com/owner/platform/releases/download") {
		t.Fatalf("resolver bypassed authenticated GitHub Release Asset API: %q", requests)
	}
}

func TestResolvePlatformReleasePaginatesMixedReleasesAndSelectsHighestCanonicalPlatformVersion(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixture(t, "1.2.3")
	metadata := resolverMetadata(t, fixture)
	older := cloneResolverJSON(t, metadata)
	older["tag_name"] = "platform-v1.2.2"
	worker := cloneResolverJSON(t, metadata)
	worker["tag_name"] = "worker-v99.0.0"
	mutable := cloneResolverJSON(t, metadata)
	mutable["tag_name"] = "platform-v9.0.0"
	mutable["immutable"] = false
	writeResolverJSON(t, fixture.releasePagePaths[0], []any{worker, older, mutable})
	writeResolverJSON(t, fixture.releasePagePaths[1], []any{metadata})
	writeResolverJSON(t, fixture.releasePagePaths[2], []any{})
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))

	output, runErr := fixture.run(t, powershell, factsPath, "", false)
	if runErr != nil || !strings.Contains(output, `"release_version":"1.2.3"`) {
		t.Fatalf("paginated mixed releases did not select highest canonical Platform version: err=%v output=%s", runErr, output)
	}
	requests, err := os.ReadFile(fixture.requestLog)
	if err != nil {
		t.Fatal(err)
	}
	for page := 1; page <= 3; page++ {
		want := "https://api.github.com/repos/owner/platform/releases?per_page=100&page=" + string(rune('0'+page))
		if !strings.Contains(string(requests), want) {
			t.Fatalf("resolver did not paginate through page %d: %q", page, requests)
		}
	}
	if strings.Contains(string(requests), "/releases/latest") {
		t.Fatalf("resolver used repository-wide latest release: %q", requests)
	}
}

func TestResolvePlatformReleaseAllowsFreshInstallToSelectExactVersionWithoutUpgradeAuthorization(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixture(t, "1.2.3")
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))

	output, runErr := fixture.run(t, powershell, factsPath, "1.2.3", false)
	if runErr != nil {
		t.Fatalf("resolve selected fresh version: %v\n%s", runErr, output)
	}
	var result struct {
		Selection      string `json:"selection"`
		ReleaseVersion string `json:"release_version"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode resolver output: %v\n%s", err, output)
	}
	if result.Selection != "explicit-version" || result.ReleaseVersion != "1.2.3" {
		t.Fatalf("unexpected resolver result: %#v", result)
	}
	requests, err := os.ReadFile(fixture.requestLog)
	if err != nil {
		t.Fatal(err)
	}
	wantMetadataURL := "https://api.github.com/repos/owner/platform/releases/tags/platform-v1.2.3"
	if !strings.Contains(string(requests), wantMetadataURL) || strings.Contains(string(requests), "/releases/latest") {
		t.Fatalf("fresh exact selection did not resolve the requested immutable tag: %q", requests)
	}
}

func TestResolvePlatformReleaseRequiresCanonicalBareSemanticVersionCore(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	for _, test := range []struct {
		version string
		accept  bool
	}{
		{version: "1.2.3", accept: true},
		{version: "2147483647.2147483647.2147483647", accept: true},
		{version: "v1.2.3"},
		{version: "01.2.3"},
		{version: "2147483648.0.0"},
	} {
		t.Run(test.version, func(t *testing.T) {
			candidateVersion := "1.2.3"
			if test.accept {
				candidateVersion = test.version
			}
			fixture := newResolverFixture(t, candidateVersion)
			factsPath := filepath.Join(fixture.directory, "host-facts.json")
			writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))

			output, runErr := fixture.run(t, powershell, factsPath, test.version, false)
			if test.accept {
				if runErr != nil {
					t.Fatalf("canonical version rejected: %v\n%s", runErr, output)
				}
				return
			}
			if runErr == nil || (!strings.Contains(output, "bare semantic version core") && !strings.Contains(output, "32-bit range")) {
				t.Fatalf("non-canonical version accepted: err=%v output=%s", runErr, output)
			}
			if requests, err := os.ReadFile(fixture.requestLog); err == nil && len(requests) != 0 {
				t.Fatalf("resolver used network before rejecting version: %q", requests)
			}
		})
	}
}

func TestResolvePlatformReleaseRequiresExactVersionWhenExistingInstallLostBothPins(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixture(t, "1.2.3")
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"workflow":{"installed":true,"trust_state":"repair_required","diagnostic":"Bootstrap Platform Release primary and backup pins are missing"},"platform":{"installation_recorded":false}}`))

	output, runErr := fixture.run(t, powershell, factsPath, "", false)
	if runErr == nil || !strings.Contains(output, "-Version <exact-installed-version>") || strings.Contains(output, `"manifest_path"`) {
		t.Fatalf("pinless existing installation was not blocked with actionable exact-version recovery: err=%v output=%s", runErr, output)
	}
	if requests, err := os.ReadFile(fixture.requestLog); err == nil && len(requests) != 0 {
		t.Fatalf("pinless existing installation resolved a mutable release before exact recovery authorization: %q", requests)
	}

	output, runErr = fixture.run(t, powershell, factsPath, "1.2.3", false)
	var result struct {
		Selection      string `json:"selection"`
		ReleaseVersion string `json:"release_version"`
	}
	if decodeErr := json.Unmarshal([]byte(output), &result); runErr != nil || decodeErr != nil || result.Selection != "explicit-version" || result.ReleaseVersion != "1.2.3" {
		t.Fatalf("explicit exact-version recovery failed: err=%v decode=%v output=%s", runErr, decodeErr, output)
	}
	requests, err := os.ReadFile(fixture.requestLog)
	wantMetadataURL := "https://api.github.com/repos/owner/platform/releases/tags/platform-v1.2.3"
	if err != nil || !strings.Contains(string(requests), wantMetadataURL) || strings.Contains(string(requests), "/releases/latest") {
		t.Fatalf("exact-version recovery did not stay on the immutable requested tag: err=%v requests=%q", err, requests)
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
		{name: "same version wrong durable manifest", candidate: "1.2.0", durableDigest: strings.Repeat("0", 64), wantError: "exact manifest digest"},
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

func TestResolvePlatformReleaseNeedsNoPlatformSigningKeyAndRejectsInvalidContract(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	t.Run("freshly installed skill has no signing key", func(t *testing.T) {
		fixture := newResolverFixture(t, "1.2.3")
		factsPath := filepath.Join(fixture.directory, "host-facts.json")
		writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
		output, runErr := fixture.run(t, powershell, factsPath, "", false)
		if runErr != nil || !strings.Contains(output, `"manifest_path"`) {
			t.Fatalf("unsigned canonical release did not resolve: err=%v output=%s", runErr, output)
		}
		if matches, _ := filepath.Glob(filepath.Join(fixture.directory, "setup-agent-workflow", "trust", "*.pem")); len(matches) != 0 {
			t.Fatalf("fixture unexpectedly required a signing key: %v", matches)
		}
	})
	t.Run("invalid platform contract", func(t *testing.T) {
		fixture := newResolverFixtureWithMutation(t, "1.2.3", func(manifest *Manifest) {
			manifest.PlatformSetup.WorkflowHomeDefault = `%TEMP%\UntrustedWorkflow`
		})
		factsPath := filepath.Join(fixture.directory, "host-facts.json")
		writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
		output, runErr := fixture.run(t, powershell, factsPath, "", false)
		if runErr == nil || !strings.Contains(output, "Workflow Home default") || strings.Contains(output, `"manifest_path"`) {
			t.Fatalf("invalid contract emitted trusted paths: err=%v output=%s", runErr, output)
		}
	})
}

func TestResolvePlatformReleaseRejectsMutableGitHubReleaseMetadata(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixture(t, "1.2.3")
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
	metadata := resolverMetadata(t, fixture)
	metadata["immutable"] = false
	writeResolverReleaseMetadata(t, fixture, metadata)

	output, runErr := fixture.run(t, powershell, factsPath, "", false)
	if runErr == nil || !strings.Contains(output, "No canonical immutable stable Platform Release was found") || strings.Contains(output, `"manifest_path"`) {
		t.Fatalf("mutable release did not fail closed before emitting paths: err=%v output=%s", runErr, output)
	}
}

func TestResolvePlatformReleaseAcceptsAdditionalReleaseAssetsWithWarnings(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixture(t, "1.2.3")
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
	metadata := resolverMetadata(t, fixture)
	metadata["assets"] = append(metadata["assets"].([]any),
		map[string]any{"id": 4, "name": "workflow-linux-amd64.tar.gz"},
		map[string]any{"id": 5, "name": "platform-sbom.spdx.json"},
		map[string]any{"id": 6, "name": "sha256sums"},
		map[string]any{"name": "metadata-missing-id.json"},
	)
	writeResolverReleaseMetadata(t, fixture, metadata)

	output, runErr := fixture.run(t, powershell, factsPath, "", false)
	if runErr != nil {
		t.Fatalf("additional release assets rejected: %v\n%s", runErr, output)
	}
	var result struct {
		Verified      bool     `json:"verified"`
		AssetWarnings []string `json:"asset_warnings"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode resolver output: %v\n%s", err, output)
	}
	if !result.Verified {
		t.Fatalf("additional release assets did not produce a verified result: %#v", result)
	}
	joinedWarnings := strings.Join(result.AssetWarnings, "\n")
	for _, expected := range []string{"workflow-linux-amd64.tar.gz", "platform-sbom.spdx.json", "sha256sums", "metadata-missing-id.json", "missing a usable asset ID"} {
		if !strings.Contains(joinedWarnings, expected) {
			t.Fatalf("additional asset warning lacks %q: %#v", expected, result.AssetWarnings)
		}
	}
}

func TestResolvePlatformReleaseRejectsMissingOrAmbiguousRequiredAssets(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing required asset", mutate: func(metadata map[string]any) {
			metadata["assets"] = metadata["assets"].([]any)[:2]
		}},
		{name: "duplicate required asset", mutate: func(metadata map[string]any) {
			metadata["assets"] = append(metadata["assets"].([]any), map[string]any{"id": 4, "name": "SHA256SUMS"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResolverFixture(t, "1.2.3")
			factsPath := filepath.Join(fixture.directory, "host-facts.json")
			writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))
			metadata := resolverMetadata(t, fixture)
			test.mutate(metadata)
			writeResolverReleaseMetadata(t, fixture, metadata)

			output, runErr := fixture.run(t, powershell, factsPath, "", false)
			if runErr == nil || !strings.Contains(output, "No canonical immutable stable Platform Release was found") || strings.Contains(output, `"manifest_path"`) {
				t.Fatalf("release with invalid required assets emitted trusted paths: err=%v output=%s", runErr, output)
			}
		})
	}
}

func TestResolvePlatformReleaseRejectsManifestFromNoncanonicalRepository(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell 5.1 is unavailable")
	}
	fixture := newResolverFixtureWithMutation(t, "1.2.3", func(manifest *Manifest) {
		manifest.Release.Repository = "attacker/platform"
		manifest.Provenance.Repository = "attacker/platform"
	})
	factsPath := filepath.Join(fixture.directory, "host-facts.json")
	writeResolverFile(t, factsPath, []byte(`{"schema_version":1,"platform":{"installation_recorded":false}}`))

	output, runErr := fixture.run(t, powershell, factsPath, "", false)
	if runErr == nil || !strings.Contains(output, "repository does not match pinned trust policy") || strings.Contains(output, `"manifest_path"`) {
		t.Fatalf("noncanonical manifest repository emitted trusted paths: err=%v output=%s", runErr, output)
	}
}

type resolverFixture struct {
	directory        string
	scriptPath       string
	policyPath       string
	manifestPath     string
	metadataPath     string
	requestLog       string
	releasePagePaths []string
}

func newResolverFixture(t *testing.T, version string) resolverFixture {
	return newResolverFixtureWithMutation(t, version, nil)
}

func newResolverFixtureWithMutation(t *testing.T, version string, mutate func(*Manifest)) resolverFixture {
	t.Helper()
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
	directory := t.TempDir()
	fixture := resolverFixture{
		directory: directory, manifestPath: filepath.Join(directory, "source-platform-release.json"),
		policyPath: filepath.Join(directory, "setup-agent-workflow", "trust", "release-policy.json"), metadataPath: filepath.Join(directory, "release.json"),
		requestLog: filepath.Join(directory, "requests.log"),
	}
	for page := 1; page <= 3; page++ {
		fixture.releasePagePaths = append(fixture.releasePagePaths, filepath.Join(directory, "releases-page-"+string(rune('0'+page))+".json"))
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
	policy, _ := json.Marshal(map[string]any{"schema_version": 1, "repository": "owner/platform", "workflow_path": manifest.Provenance.WorkflowPath, "minimum_platform_version": "0.0.0"})
	writeResolverFile(t, fixture.policyPath, policy)
	assetNames := []string{"SHA256SUMS", "platform-release.json", "workflow-windows-amd64.zip"}
	assets := make([]map[string]any, 0, len(assetNames))
	for index, name := range assetNames {
		assets = append(assets, map[string]any{"id": index + 1, "name": name})
	}
	metadata, _ := json.Marshal(map[string]any{"tag_name": manifest.Release.Tag, "draft": false, "prerelease": false, "immutable": true, "target_commitish": manifest.Release.SourceCommit, "assets": assets})
	writeResolverFile(t, fixture.metadataPath, metadata)
	writeResolverJSON(t, fixture.releasePagePaths[0], []any{json.RawMessage(metadata)})
	writeResolverJSON(t, fixture.releasePagePaths[1], []any{})
	writeResolverJSON(t, fixture.releasePagePaths[2], []any{})
	return fixture
}

func resolverMetadata(t *testing.T, fixture resolverFixture) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(fixture.metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func writeResolverJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeResolverFile(t, path, raw)
}

func writeResolverReleaseMetadata(t *testing.T, fixture resolverFixture, metadata map[string]any) {
	t.Helper()
	writeResolverJSON(t, fixture.metadataPath, metadata)
	writeResolverJSON(t, fixture.releasePagePaths[0], []any{metadata})
}

func cloneResolverJSON(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func (fixture resolverFixture) run(t *testing.T, powershell, factsPath, version string, allowUpgrade bool) (string, error) {
	t.Helper()
	wrapper := filepath.Join(fixture.directory, "invoke-resolver.ps1")
	writeResolverFile(t, wrapper, []byte(`
function Invoke-WebRequest {
    param([string]$Uri, $Headers, [string]$OutFile, [switch]$UseBasicParsing)
    if ([string]$Headers.Authorization -cne ("Bearer " + $env:WORKFLOW_TEST_EXPECTED_PAT)) { throw "unexpected Authorization header" }
    Add-Content -LiteralPath $env:WORKFLOW_TEST_REQUEST_LOG -Value $Uri
    if ([string]::IsNullOrWhiteSpace($OutFile)) {
		if ($Uri.StartsWith('https://api.github.com/repos/owner/platform/releases?per_page=100&page=', [StringComparison]::Ordinal)) {
			$page = [int]$Uri.Substring($Uri.LastIndexOf('=') + 1)
			$pages = @($env:WORKFLOW_TEST_RELEASE_PAGE_1, $env:WORKFLOW_TEST_RELEASE_PAGE_2, $env:WORKFLOW_TEST_RELEASE_PAGE_3)
			if ($page -le $pages.Count) { $source = $pages[$page - 1] }
			else { return [pscustomobject]@{ Content = '[]' } }
		}
		elseif ($Uri -match '/actions/runs/') { $source = $env:WORKFLOW_TEST_RUN }
        elseif ($Uri -match '/commits/.+/pulls$') { $source = $env:WORKFLOW_TEST_COMMIT_PULLS }
        elseif ($Uri -match '/pulls/[0-9]+$') { $source = $env:WORKFLOW_TEST_PULL }
		elseif ($Uri -match '/releases/tags/') { $source = $env:WORKFLOW_TEST_METADATA }
		else { throw "unexpected API request $Uri" }
        return [pscustomobject]@{ Content = [IO.File]::ReadAllText($source) }
    }
	if ($Uri -match '/releases/assets/[0-9]+$') { Copy-Item -LiteralPath $env:WORKFLOW_TEST_MANIFEST -Destination $OutFile }
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
	command.Stdin = strings.NewReader("ghp_test_release_resolution\n")
	allow := "0"
	if allowUpgrade {
		allow = "1"
	}
	command.Env = append(os.Environ(),
		"WORKFLOW_TEST_REQUEST_LOG="+fixture.requestLog,
		"WORKFLOW_TEST_METADATA="+fixture.metadataPath,
		"WORKFLOW_TEST_RELEASE_PAGE_1="+fixture.releasePagePaths[0],
		"WORKFLOW_TEST_RELEASE_PAGE_2="+fixture.releasePagePaths[1],
		"WORKFLOW_TEST_RELEASE_PAGE_3="+fixture.releasePagePaths[2],
		"WORKFLOW_TEST_MANIFEST="+fixture.manifestPath,
		"WORKFLOW_TEST_FACTS="+factsPath,
		"WORKFLOW_TEST_VERSION="+version,
		"WORKFLOW_TEST_ALLOW_UPGRADE="+allow,
		"WORKFLOW_TEST_EXPECTED_PAT=ghp_test_release_resolution",
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
