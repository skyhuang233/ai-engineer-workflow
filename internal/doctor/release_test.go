package doctor

import "testing"

func TestWorkerReleaseManifestBindsAcceptedInputsToPublishedDigest(t *testing.T) {
	config := validConfig()
	manifest := WorkerReleaseManifest{
		SchemaVersion:                1,
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
