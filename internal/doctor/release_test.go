package doctor

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestWorkerBuildInputIdentityUsesNewlineFreeCanonicalJSON(t *testing.T) {
	config := validConfig()
	workerTree := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	publisherWorkflow := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	canonical := fmt.Sprintf(`{"schema_version":1,"deploy_worker_tree":%q,"publish_worker_workflow_blob":%q,"codex":{"version":%q},"no_mistakes":{"version":%q,"upstream_repository":%q,"upstream_commit":%q,"fork_repository":%q,"fork_release":%q,"linux_amd64_sha256":%q},"worker":{"version":%q,"image_repository":%q,"release_repository":%q}}`,
		workerTree, publisherWorkflow, config.Codex.Version, config.NoMistakes.Version,
		config.NoMistakes.UpstreamRepository, config.NoMistakes.UpstreamCommit,
		config.NoMistakes.ForkRepository, config.NoMistakes.ForkRelease,
		config.NoMistakes.LinuxAMD64SHA256, config.Worker.Version,
		config.Worker.ImageRepository, config.Worker.ReleaseRepository)
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	if got := workerBuildInputIdentity(config, workerTree, publisherWorkflow); got != want {
		t.Fatalf("identity = %s, want canonical newline-free SHA-256 %s", got, want)
	}
}

func TestWorkerReleaseManifestBindsAcceptedInputsToPublishedDigest(t *testing.T) {
	config := validConfig()
	manifest := WorkerReleaseManifest{
		SchemaVersion:                2,
		WorkerVersion:                config.Worker.Version,
		SourceCommit:                 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Image:                        config.Worker.ImageRepository + "@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CodexVersion:                 config.Codex.Version,
		NoMistakesVersion:            config.NoMistakes.Version,
		NoMistakesUpstreamRepository: config.NoMistakes.UpstreamRepository,
		NoMistakesCommit:             config.NoMistakes.UpstreamCommit,
		NoMistakesForkRepository:     config.NoMistakes.ForkRepository,
		NoMistakesForkRelease:        config.NoMistakes.ForkRelease,
		NoMistakesLinuxAMD64SHA256:   config.NoMistakes.LinuxAMD64SHA256,
		BuildInputIdentity:           "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		GitHubActionsRunID:           123,
	}
	if err := manifest.Validate(config); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	manifest.SourceCommit = "main"
	if err := manifest.Validate(config); err == nil {
		t.Fatal("manifest accepted a floating source revision")
	}
	manifest.SourceCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest.NoMistakesForkRelease = "wrong-release"
	if err := manifest.Validate(config); err == nil {
		t.Fatal("manifest accepted a different no-mistakes fork release")
	}
	manifest.NoMistakesForkRelease = config.NoMistakes.ForkRelease
	manifest.NoMistakesForkRepository = "wrong/repository"
	if err := manifest.Validate(config); err == nil {
		t.Fatal("manifest accepted a different no-mistakes fork repository")
	}
	manifest.NoMistakesForkRepository = config.NoMistakes.ForkRepository
	manifest.NoMistakesUpstreamRepository = "wrong/repository"
	if err := manifest.Validate(config); err == nil {
		t.Fatal("manifest accepted a different no-mistakes upstream repository")
	}
}
