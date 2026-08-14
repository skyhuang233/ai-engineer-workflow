package workflowhome

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcilePathAddsBinOnceCaseInsensitively(t *testing.T) {
	bin := `C:\Users\Me\AgentWorkflow\bin`
	got := ReconcilePath(`C:\Windows;C:\USERS\ME\AGENTWORKFLOW\BIN;C:\Tools`, bin)
	if strings.Count(strings.ToLower(got), strings.ToLower(bin)) != 1 {
		t.Fatalf("PATH = %q", got)
	}
	without := ReconcilePath(`C:\Windows;C:\Tools`, bin)
	if !strings.HasSuffix(without, bin) {
		t.Fatalf("PATH = %q", without)
	}
}

func TestInstallVersionPublishesOwnedCurrentExecutable(t *testing.T) {
	layout, err := Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(source, []byte("version-one"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("version-one"))
	checksum := hex.EncodeToString(digest[:])
	installation := Installation{Layout: layout}
	if err := installation.InstallVersion("1.2.3", source, checksum); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(layout.Bin, ExecutableName))
	if err != nil || string(current) != "version-one" {
		t.Fatalf("current = %q, %v", current, err)
	}
	if err := installation.InstallVersion("1.2.3", source, checksum); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Bin, OwnershipFile), []byte(`{"owner":"somebody-else"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installation.InstallVersion("1.2.4", source, checksum); err == nil {
		t.Fatal("unowned executable was overwritten")
	}
}
