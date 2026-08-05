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
		"snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}",
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
}
