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
	if got, err := normalizeWindowsNamespacePath(`\\?\UNC\server\share\workflow.db`); err != nil || got != `\\server\share\workflow.db` {
		want := `\\server\share\workflow.db`
		t.Fatalf("UNC namespace path = %q, want %q", got, want)
	}
	if got, err := normalizeWindowsNamespacePath(`//?/UNC/server/share/workflow.db`); err != nil || got != `\\server\share\workflow.db` {
		want := `\\server\share\workflow.db`
		t.Fatalf("forward-slash UNC namespace path = %q, want %q", got, want)
	}
	uncPath, err := normalizeWindowsNamespacePath(`\\?\UNC\SERVER\SHARE\CaseSensitive\Workflow.db`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeWindowsVolumeCase(uncPath), `\\server\share\CaseSensitive\Workflow.db`; got != want {
		t.Fatalf("UNC volume identity = %q, want %q", got, want)
	}
}

func TestDatabaseIdentityRejectsUnsupportedWindowsDeviceNamespaces(t *testing.T) {
	for _, path := range []string{
		`\\?\GLOBALROOT\Device\HarddiskVolume1\workflow.db`,
		`\\.\GLOBALROOT\Device\HarddiskVolume1\workflow.db`,
		`\??\GLOBALROOT\Device\HarddiskVolume1\workflow.db`,
	} {
		if _, err := DatabaseIdentity(path); err == nil || !strings.Contains(err.Error(), "unsupported Windows device namespace") {
			t.Fatalf("DatabaseIdentity(%q) error = %v, want unsupported namespace error", path, err)
		}
	}
}

func TestDatabaseIdentityRejectsNamespaceOnlyPathSemantics(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	for _, path := range []string{
		`\\?\` + root + `.`,
		`\\?\` + root + ` `,
		`\\?\` + root + `\.\workflow.db`,
		`\\?\` + root + `\..\workflow.db`,
		`\\?\` + root + `\CON.db`,
		`\\?\UNC\server\share\workflow.db.`,
		`\\?\UNC\server\share.\workflow.db`,
	} {
		if _, err := DatabaseIdentity(path); err == nil || !strings.Contains(err.Error(), "unsupported Windows namespace path component") {
			t.Fatalf("DatabaseIdentity(%q) error = %v, want namespace component error", path, err)
		}
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

func TestDatabaseIdentityKeepsStableWindowsCaseWithMissingParent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "New", "Workflow.db")
	beforeCreate, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	afterParentCreate, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	afterDatabaseCreate, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	for state, identity := range map[string]string{
		"parent creation":   afterParentCreate,
		"database creation": afterDatabaseCreate,
		"deletion":          afterDelete,
	} {
		if identity != beforeCreate {
			t.Fatalf("identity after %s = %q, want %q", state, identity, beforeCreate)
		}
	}
}
