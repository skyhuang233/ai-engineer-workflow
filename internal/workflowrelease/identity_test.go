package workflowrelease

import (
	"os"
	"strings"
	"testing"
)

func testBuildInput() BuildInput {
	return BuildInput{
		SchemaVersion: 1,
		GitInputs: GitInputs{
			DeployWorkerTree:         strings.Repeat("1", 40),
			DeliverySourceDigestTree: strings.Repeat("2", 40),
			DeliverySourceTree:       strings.Repeat("3", 40),
			GoModBlob:                strings.Repeat("4", 40),
			GoSumBlob:                strings.Repeat("5", 40),
			PublishWorkflowBlob:      strings.Repeat("6", 40),
		},
		Toolchain: Tools{
			Codex:     CodexTool{Version: "0.147.0"},
			GitHubCLI: ArchiveTool{Version: "2.97.0", LinuxAMD64SHA256: strings.Repeat("a", 64)},
			Go:        ArchiveTool{Version: "1.25.12", LinuxAMD64SHA256: strings.Repeat("b", 64)},
			NoMistakes: NoMistakesTool{
				Version: "v1.41.2", UpstreamRepository: "kunchenguid/no-mistakes", UpstreamCommit: strings.Repeat("7", 40),
				ForkRepository: "skyhuang233/no-mistakes", ForkCommit: strings.Repeat("8", 40), ForkRelease: "workflow-v1.41.2.3", LinuxAMD64SHA256: strings.Repeat("c", 64),
			},
		},
		Worker: BuildWorker{ImageRepository: WorkerRepository},
	}
}

func TestBuildInputIdentityUsesTheSchemaOneByteContract(t *testing.T) {
	input := testBuildInput()
	canonical, err := input.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := `{"schema_version":1,"git_inputs":{"deploy_worker_tree":"1111111111111111111111111111111111111111","delivery_source_digest_tree":"2222222222222222222222222222222222222222","delivery_source_tree":"3333333333333333333333333333333333333333","go_mod_blob":"4444444444444444444444444444444444444444","go_sum_blob":"5555555555555555555555555555555555555555","publish_workflow_blob":"6666666666666666666666666666666666666666"},"toolchain":{"codex":{"version":"0.147.0"},"github_cli":{"version":"2.97.0","linux_amd64_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"go":{"version":"1.25.12","linux_amd64_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"no_mistakes":{"version":"v1.41.2","upstream_repository":"kunchenguid/no-mistakes","upstream_commit":"7777777777777777777777777777777777777777","fork_repository":"skyhuang233/no-mistakes","fork_commit":"8888888888888888888888888888888888888888","fork_release":"workflow-v1.41.2.3","linux_amd64_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},"worker":{"image_repository":"ghcr.io/skyhuang233/workflow-worker"}}`
	if string(canonical) != wantCanonical {
		t.Fatalf("canonical build input differs\ngot:  %s\nwant: %s", canonical, wantCanonical)
	}
	identity, err := input.Identity()
	if err != nil {
		t.Fatal(err)
	}
	const wantIdentity = "1900b59b7ee22bbe2d9e337cb959e066e3034033efa6c476c93c7af4af43975a"
	if identity != wantIdentity {
		t.Fatalf("identity = %s, want %s", identity, wantIdentity)
	}
}

func TestDecodeBuildInputRejectsNonCanonicalInputs(t *testing.T) {
	raw, err := testBuildInput().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for name, changed := range map[string]string{
		"unknown field":    strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"extra":true`, 1),
		"uppercase oid":    strings.Replace(string(raw), strings.Repeat("1", 40), strings.Repeat("A", 40), 1),
		"wrong repository": strings.Replace(string(raw), WorkerRepository, "ghcr.io/other/worker", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeBuildInput([]byte(changed)); err == nil {
				t.Fatalf("DecodeBuildInput accepted %s", name)
			}
		})
	}
}

func TestLoadToolchainOmitsAComponentVersionAndSuppliesIdentityTools(t *testing.T) {
	config, err := LoadToolchain("../../config/toolchain.json")
	if err != nil {
		t.Fatal(err)
	}
	if config.Worker.ImageRepository != WorkerRepository {
		t.Fatalf("image repository = %q", config.Worker.ImageRepository)
	}
	if err := config.Tools().Validate(); err != nil {
		t.Fatalf("toolchain tools: %v", err)
	}
}

func TestDecodeToolchainRejectsUnknownIdentityInputs(t *testing.T) {
	raw, err := os.ReadFile("../../config/toolchain.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `"version": "0.147.0"`, `"version": "0.147.0", "unhashed_option": true`, 1)
	if _, err := DecodeToolchain([]byte(changed)); err == nil {
		t.Fatal("DecodeToolchain accepted an unhashed Codex option")
	}
}
