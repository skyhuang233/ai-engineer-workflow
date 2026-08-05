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
		"APT_PACKAGES=\"ca-certificates=20230311+deb12u1 curl=7.88.1-10+deb12u15 gh=2.23.0+dfsg1-1 git=1:2.39.5-0+deb12u3 jq=1.6-2.1+deb12u2 sqlite3=3.40.1-2+deb12u2\"",
		"io.workflow.debian.snapshot",
		"io.workflow.apt.packages",
		"NO_MISTAKES_UPSTREAM_COMMIT",
		"io.workflow.no-mistakes.upstream-commit",
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("Worker Dockerfile omits immutable provenance input %q", required)
		}
	}
	if strings.Contains(contents, "http://snapshot.debian.org") {
		t.Fatal("Worker Dockerfile uses an insecure Debian snapshot transport")
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
	} {
		if !strings.Contains(string(contents), required) {
			t.Fatalf("Worker contract omits immutable no-mistakes build check %q", required)
		}
	}
}
