package workflowhome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultsToLocalAppData(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	layout, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := filepath.Abs(filepath.Join(os.Getenv("LOCALAPPDATA"), "AgentWorkflow"))
	if layout.Root != expected {
		t.Fatalf("Root = %q, want %q", layout.Root, expected)
	}
	if layout.CredentialFile != filepath.Join(layout.State, "credentials", "github.pat") {
		t.Fatalf("CredentialFile = %q", layout.CredentialFile)
	}
}

func TestResolveRejectsRelativeAndUNCOverrides(t *testing.T) {
	for _, input := range []string{"relative", `\\server\share\workflow`} {
		if _, err := Resolve(input); err == nil {
			t.Fatalf("Resolve(%q) succeeded", input)
		}
	}
}

func TestEnsureCreatesCompleteLayoutIdempotently(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	layout, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := layout.Ensure(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range layout.Directories() {
		if info, err := filepath.Glob(path); err != nil || len(info) != 1 {
			t.Fatalf("directory %q not created", path)
		}
	}
}
