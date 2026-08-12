//go:build windows

package startup

import (
	"os"
	"path/filepath"
	"strings"
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
	if got, want := normalizeWindowsVolumeCase(normalizeWindowsNamespacePath(`\\?\UNC\SERVER\SHARE\CaseSensitive\Workflow.db`)), `\\server\share\CaseSensitive\Workflow.db`; got != want {
		t.Fatalf("UNC volume identity = %q, want %q", got, want)
	}
}

func TestDatabaseIdentityNormalizesWindowsVolumeGUIDAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	plain, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	mountPoint, err := windowsVolumeMountPoint(path)
	if err != nil {
		t.Fatal(err)
	}
	volumeName, err := windowsVolumeName(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(mountPoint, path)
	if err != nil {
		t.Fatal(err)
	}
	volumeAlias := filepath.Join(volumeName, relative)
	for _, alias := range []string{
		volumeAlias,
		strings.Replace(volumeAlias, `\\?\`, `\\.\`, 1),
		strings.Replace(volumeAlias, `\\?\`, `\??\`, 1),
	} {
		identity, err := DatabaseIdentity(alias)
		if err != nil {
			t.Fatal(err)
		}
		if identity != plain {
			t.Fatalf("volume alias %q identity = %q, want %q", alias, identity, plain)
		}
	}
}

func TestDatabaseIdentityKeepsStableWindowsCaseAcrossLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Workflow.db")
	beforeCreate, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	afterCreate, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterCreate != beforeCreate {
		t.Fatalf("existing database identity = %q, want pre-creation identity %q", afterCreate, beforeCreate)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete != beforeCreate {
		t.Fatalf("deleted database identity = %q, want pre-creation identity %q", afterDelete, beforeCreate)
	}
	caseSensitive, known, err := windowsDirectoryCaseSensitivity(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !known || caseSensitive {
		return
	}
	if got, want := filepath.Base(beforeCreate), "workflow.db"; got != want {
		t.Fatalf("case-insensitive database identity base = %q, want %q", got, want)
	}
	alias, err := DatabaseIdentity(filepath.Join(filepath.Dir(path), "WORKFLOW.DB"))
	if err != nil {
		t.Fatal(err)
	}
	if alias != beforeCreate {
		t.Fatalf("case-insensitive alias identity = %q, want %q", alias, beforeCreate)
	}
}
