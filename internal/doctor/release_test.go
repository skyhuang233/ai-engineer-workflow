package doctor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestWorkerBuildInputIdentityUsesCanonicalBase64JSON(t *testing.T) {
	config := validConfig()
	config.NoMistakes.ForkRelease = "worker&release"
	workerTree := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	publisherWorkflow := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	b64 := base64.StdEncoding.EncodeToString
	canonical := fmt.Sprintf(`{"schema_version":2,"deploy_worker_tree":%q,"publish_worker_workflow_blob":%q,"codex":{"version":%q},"no_mistakes":{"version":%q,"upstream_repository":%q,"upstream_commit":%q,"fork_repository":%q,"fork_release":%q,"linux_amd64_sha256":%q},"worker":{"version":%q,"image_repository":%q,"release_repository":%q}}`,
		b64([]byte(workerTree)), b64([]byte(publisherWorkflow)), b64([]byte(config.Codex.Version)), b64([]byte(config.NoMistakes.Version)),
		b64([]byte(config.NoMistakes.UpstreamRepository)), b64([]byte(config.NoMistakes.UpstreamCommit)),
		b64([]byte(config.NoMistakes.ForkRepository)), b64([]byte(config.NoMistakes.ForkRelease)),
		b64([]byte(config.NoMistakes.LinuxAMD64SHA256)), b64([]byte(config.Worker.Version)),
		b64([]byte(config.Worker.ImageRepository)), b64([]byte(config.Worker.ReleaseRepository)))
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	if got := workerBuildInputIdentity(config, workerTree, publisherWorkflow); got != want {
		t.Fatalf("identity = %s, want canonical newline-free SHA-256 %s", got, want)
	}
}

func TestWorkerReleaseTagIncludesVersionAndBuildInputIdentity(t *testing.T) {
	identity := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if got, want := workerReleaseTag("0.1.0", identity), "worker-v0.1.0-"+identity; got != want {
		t.Fatalf("release tag = %q, want %q", got, want)
	}
	config := validConfig()
	config.Worker.Version = strings.Repeat("a", 56)
	if err := config.Validate(); err == nil {
		t.Fatal("accepted a Worker version too long for a source-keyed image tag")
	}
}

func TestWorkerBuildInputIdentityMatchesPublisherJQ(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required by the GitHub publisher workflow")
	}
	config := validConfig()
	config.NoMistakes.ForkRelease = "worker&release"
	workerTree := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	publisherWorkflow := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	filter := `{schema_version:2,deploy_worker_tree:($deploy_worker_tree | @base64),publish_worker_workflow_blob:($publish_worker_workflow_blob | @base64),codex:{version:(.codex.version | @base64)},no_mistakes:{version:(.no_mistakes.version | @base64),upstream_repository:(.no_mistakes.upstream_repository | @base64),upstream_commit:(.no_mistakes.upstream_commit | @base64),fork_repository:(.no_mistakes.fork_repository | @base64),fork_release:(.no_mistakes.fork_release | @base64),linux_amd64_sha256:(.no_mistakes.linux_amd64_sha256 | @base64)},worker:{version:(.worker.version | @base64),image_repository:(.worker.image_repository | @base64),release_repository:(.worker.release_repository | @base64)}}`
	command := exec.Command(jq, "--compact-output", "--arg", "deploy_worker_tree", workerTree, "--arg", "publish_worker_workflow_blob", publisherWorkflow, filter)
	command.Stdin = bytes.NewReader(configJSON)
	publisherJSON, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	publisherCanonicalJSON := strings.TrimRight(string(publisherJSON), "\r\n")
	goJSON, err := json.Marshal(canonicalizeWorkerBuildInputs(workerBuildInputs{
		SchemaVersion:             2,
		DeployWorkerTree:          workerTree,
		PublishWorkerWorkflowBlob: publisherWorkflow,
		Codex:                     config.Codex,
		NoMistakes:                config.NoMistakes,
		Worker:                    config.Worker,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(goJSON) != publisherCanonicalJSON {
		t.Fatalf("Go canonical JSON = %s, publisher canonical JSON = %s", goJSON, publisherCanonicalJSON)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(publisherCanonicalJSON)))
	if got := workerBuildInputIdentity(config, workerTree, publisherWorkflow); got != want {
		t.Fatalf("Go identity = %s, publisher jq identity = %s", got, want)
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
