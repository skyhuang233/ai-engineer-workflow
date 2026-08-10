package doctor

import (
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
	dockerfile, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "deploy", "worker", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	for _, required := range []string{
		"DEBIAN_SNAPSHOT=20260713T000000Z",
		"https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}",
		"apt-get update -o Acquire::Check-Valid-Until=false -o Acquire::https::Verify-Peer=false",
		"apt-get install --yes --no-install-recommends -o Acquire::https::Verify-Peer=false ca-certificates=20230311+deb12u1",
		"APT_PACKAGES=\"ca-certificates=20230311+deb12u1 curl=7.88.1-10+deb12u15 git=1:2.39.5-0+deb12u3 jq=1.6-2.1+deb12u2 procps=2:4.0.2-3 sqlite3=3.40.1-2+deb12u2\"",
		"io.workflow.debian.snapshot",
		"io.workflow.apt.packages",
		"GO_VERSION=1.25.12",
		"GO_LINUX_AMD64_SHA256=234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1",
		"io.workflow.go.version",
		"go version",
		"GITHUB_CLI_VERSION=2.97.0",
		"GITHUB_CLI_LINUX_AMD64_SHA256=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112",
		"https://github.com/cli/cli/releases/download/v${GITHUB_CLI_VERSION}/gh_${GITHUB_CLI_VERSION}_linux_amd64.tar.gz",
		"io.workflow.github-cli.version",
		"gh version",
		"rm --recursive --force /usr/local/lib/node_modules/npm /usr/local/bin/npm /usr/local/bin/npx",
		"test ! -e /usr/local/lib/node_modules/npm",
		"NO_MISTAKES_UPSTREAM_COMMIT",
		"io.workflow.no-mistakes.upstream-commit",
		"NO_MISTAKES_FORK_COMMIT=e073fd0dc51c64004468b04de8cf2ab50cd5d177",
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
		"no-mistakes daemon status",
	} {
		if !strings.Contains(string(contents), required) {
			t.Fatalf("Worker contract omits immutable no-mistakes build check %q", required)
		}
	}
}
