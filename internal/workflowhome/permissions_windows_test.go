//go:build windows

package workflowhome

import (
	"path/filepath"
	"testing"
)

func TestSecureCredentialPathValidatesLocalAbsolutePathWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "credentials", "github.pat")
	if err := SecureCredentialPath(path, false); err != nil {
		t.Fatal(err)
	}
	if err := SecureCredentialPath("relative\\github.pat", false); err == nil {
		t.Fatal("relative credential path accepted")
	}
}
