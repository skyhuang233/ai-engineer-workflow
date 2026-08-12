package startup

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDatabaseIdentityNormalizesWindowsNamespaceAliases(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path namespace semantics are required")
	}
	path := filepath.Join(t.TempDir(), "workflow.db")
	plain, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	namespaced, err := DatabaseIdentity(`\\?\` + path)
	if err != nil {
		t.Fatal(err)
	}
	if namespaced != plain {
		t.Fatalf("namespace identity = %q, want %q", namespaced, plain)
	}
	if got, want := normalizeWindowsNamespacePath(`\\?\UNC\server\share\workflow.db`), `\\server\share\workflow.db`; got != want {
		t.Fatalf("UNC namespace path = %q, want %q", got, want)
	}
}
