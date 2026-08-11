package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkerDockerfilePinsAPTInputsAndNoMistakesCommit(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	repositoryRoot := filepath.Join(filepath.Dir(file), "..", "..")
	dockerfile, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy", "worker", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(filepath.Join(repositoryRoot, "config", "toolchain.json"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	for _, required := range []string{
		fmt.Sprintf("ARG CODEX_VERSION=%s", config.Codex.Version),
		fmt.Sprintf("ARG GITHUB_CLI_VERSION=%s", config.GitHubCLI.Version),
		fmt.Sprintf("ARG GITHUB_CLI_LINUX_AMD64_SHA256=%s", config.GitHubCLI.LinuxAMD64SHA256),
		fmt.Sprintf("ARG GO_VERSION=%s", config.Go.Version),
		fmt.Sprintf("ARG GO_LINUX_AMD64_SHA256=%s", config.Go.LinuxAMD64SHA256),
		fmt.Sprintf("ARG NO_MISTAKES_VERSION=%s", config.NoMistakes.Version),
		fmt.Sprintf("ARG NO_MISTAKES_UPSTREAM_COMMIT=%s", config.NoMistakes.UpstreamCommit),
		fmt.Sprintf("ARG NO_MISTAKES_FORK_REPOSITORY=%s", config.NoMistakes.ForkRepository),
		fmt.Sprintf("ARG NO_MISTAKES_FORK_COMMIT=%s", config.NoMistakes.ForkCommit),
		fmt.Sprintf("ARG NO_MISTAKES_FORK_RELEASE=%s", config.NoMistakes.ForkRelease),
		fmt.Sprintf("ARG NO_MISTAKES_SHA256=%s", config.NoMistakes.LinuxAMD64SHA256),
		"DEBIAN_SNAPSHOT=20260713T000000Z",
		"https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}",
		"apt-get update -o Acquire::Check-Valid-Until=false -o Acquire::https::Verify-Peer=false",
		"apt-get install --yes --no-install-recommends -o Acquire::https::Verify-Peer=false ca-certificates=20230311+deb12u1",
		"APT_PACKAGES=\"ca-certificates=20230311+deb12u1 curl=7.88.1-10+deb12u15 git=1:2.39.5-0+deb12u3 jq=1.6-2.1+deb12u2 procps=2:4.0.2-3 sqlite3=3.40.1-2+deb12u2\"",
		"io.workflow.debian.snapshot",
		"io.workflow.apt.packages",
		"io.workflow.go.version",
		"go version",
		"https://github.com/cli/cli/releases/download/v${GITHUB_CLI_VERSION}/gh_${GITHUB_CLI_VERSION}_linux_amd64.tar.gz",
		"io.workflow.github-cli.version",
		"gh version",
		"rm --recursive --force /usr/local/lib/node_modules/npm /usr/local/bin/npm /usr/local/bin/npx",
		"test ! -e /usr/local/lib/node_modules/npm",
		"io.workflow.no-mistakes.upstream-commit",
		"io.workflow.no-mistakes.fork-commit",
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("Worker Dockerfile omits immutable provenance input %q", required)
		}
	}
	if strings.Contains(contents, "http://snapshot.debian.org") {
		t.Fatal("Worker Dockerfile uses an insecure Debian snapshot transport")
	}
	if strings.Contains(contents, "gh=2.23.0") {
		t.Fatal("Worker Dockerfile still installs the vulnerable Debian GitHub CLI")
	}
}

func TestWorkerContractRequiresCleanNoMistakesBuild(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	contents, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "worker-contract.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"vcs\\.revision",
		"vcs\\.modified",
		"test \"${embedded_no_mistakes_modified[0]}\" = false",
		"no-mistakes daemon start",
		"daemon_status=\"$(no-mistakes daemon status)\"",
		"*\"daemon running\"*",
	} {
		if !strings.Contains(string(contents), required) {
			t.Fatalf("Worker contract omits immutable no-mistakes build check %q", required)
		}
	}
}
