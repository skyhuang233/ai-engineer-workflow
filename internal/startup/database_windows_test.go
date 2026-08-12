//go:build windows

package startup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseIdentityNormalizesWindowsNamespaceAliases(t *testing.T) {
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
	forwardNamespaced, err := DatabaseIdentity(`//?/` + filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if forwardNamespaced != plain {
		t.Fatalf("forward-slash namespace identity = %q, want %q", forwardNamespaced, plain)
	}
	if got, want := normalizeWindowsNamespacePath(`\\?\UNC\server\share\workflow.db`), `\\server\share\workflow.db`; got != want {
		t.Fatalf("UNC namespace path = %q, want %q", got, want)
	}
	if got, want := normalizeWindowsNamespacePath(`//?/UNC/server/share/workflow.db`), `\\server\share\workflow.db`; got != want {
		t.Fatalf("forward-slash UNC namespace path = %q, want %q", got, want)
	}
}

func TestDatabaseIdentityPreservesCanonicalWindowsCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Workflow.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(identity), filepath.Base(path); got != want {
		t.Fatalf("database identity base = %q, want canonical case %q", got, want)
	}
	caseSensitive, known, err := windowsDirectoryCaseSensitivity(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !known || caseSensitive {
		return
	}
	alias, err := DatabaseIdentity(filepath.Join(filepath.Dir(path), "WORKFLOW.DB"))
	if err != nil {
		t.Fatal(err)
	}
	if alias != identity {
		t.Fatalf("case-insensitive alias identity = %q, want %q", alias, identity)
	}
}
