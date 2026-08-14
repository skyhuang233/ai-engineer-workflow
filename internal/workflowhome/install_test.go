package workflowhome

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memoryPathStore struct{ value string }

func (s *memoryPathStore) Load() (string, error) { return s.value, nil }
func (s *memoryPathStore) Save(value string) error {
	s.value = value
	return nil
}

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

func TestPersistPathReconcilesTheCurrentUserValueIdempotently(t *testing.T) {
	store := &memoryPathStore{value: `C:\Windows;C:\USERS\ME\AGENTWORKFLOW\BIN;C:\Users\Me\AgentWorkflow\bin;C:\Tools`}
	bin := `C:\Users\Me\AgentWorkflow\bin`
	if err := PersistPath(store, bin); err != nil {
		t.Fatal(err)
	}
	first := store.value
	if strings.Count(strings.ToLower(first), strings.ToLower(bin)) != 1 {
		t.Fatalf("persisted PATH = %q", first)
	}
	if err := PersistPath(store, bin); err != nil {
		t.Fatal(err)
	}
	if store.value != first {
		t.Fatalf("second reconciliation changed PATH: %q -> %q", first, store.value)
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

func TestInstallSkillBundleVerifiesExactFilesAndSwitchesOwnedVersion(t *testing.T) {
	layout, err := Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "skills")
	installation := Installation{Layout: layout}

	install := func(version, contents string) {
		t.Helper()
		source := filepath.Join(t.TempDir(), "bundle")
		if err := os.MkdirAll(filepath.Join(source, "agent-workflow"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "agent-workflow", "SKILL.md"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(contents))
		spec := SkillBundleSpec{
			Version:         version,
			DestinationRoot: destination,
			ManagedSkills:   []string{"agent-workflow"},
			Files: []SkillBundleFile{{
				Path: "agent-workflow/SKILL.md", SHA256: hex.EncodeToString(digest[:]),
			}},
		}
		if err := installation.InstallSkillBundle(source, spec); err != nil {
			t.Fatal(err)
		}
	}

	install("1.0.0", "version one")
	install("1.1.0", "version two")
	digest := sha256.Sum256([]byte("version two"))
	currentSpec := SkillBundleSpec{Version: "1.1.0", DestinationRoot: destination, ManagedSkills: []string{"agent-workflow"}, Files: []SkillBundleFile{{Path: "agent-workflow/SKILL.md", SHA256: hex.EncodeToString(digest[:])}}}
	verified, err := installation.VerifySkillBundle(currentSpec)
	if err != nil || !verified {
		t.Fatalf("installed Workflow Skill Bundle verified=%t, %v", verified, err)
	}
	installed, err := os.ReadFile(filepath.Join(destination, "agent-workflow", "SKILL.md"))
	if err != nil || string(installed) != "version two" {
		t.Fatalf("installed skill = %q, %v", installed, err)
	}
	state, err := os.ReadFile(filepath.Join(layout.Config, SkillBundleOwnershipFile))
	if err != nil || !strings.Contains(string(state), `"version":"1.1.0"`) {
		t.Fatalf("bundle ownership = %q, %v", state, err)
	}
}

func TestInstallSkillBundleBlocksUnownedNamesAndDigestDrift(t *testing.T) {
	layout, err := Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "skills")
	source := filepath.Join(t.TempDir(), "bundle")
	for _, root := range []string{filepath.Join(destination, "agent-workflow"), filepath.Join(source, "agent-workflow")} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(destination, "agent-workflow", "SKILL.md"), []byte("user-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent-workflow", "SKILL.md"), []byte("platform"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("platform"))
	spec := SkillBundleSpec{Version: "1.0.0", DestinationRoot: destination, ManagedSkills: []string{"agent-workflow"}, Files: []SkillBundleFile{{Path: "agent-workflow/SKILL.md", SHA256: hex.EncodeToString(digest[:])}}}
	installation := Installation{Layout: layout}
	if err := installation.InstallSkillBundle(source, spec); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unowned skill error = %v", err)
	}
	if installed, _ := os.ReadFile(filepath.Join(destination, "agent-workflow", "SKILL.md")); string(installed) != "user-owned" {
		t.Fatalf("unowned skill was changed to %q", installed)
	}

	if err := os.RemoveAll(filepath.Join(destination, "agent-workflow")); err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("different bytes"))
	spec.Files[0].SHA256 = hex.EncodeToString(wrong[:])
	if err := installation.InstallSkillBundle(source, spec); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("digest drift error = %v", err)
	}
}

func TestVerifyRecordedSkillBundleRechecksInstalledFiles(t *testing.T) {
	layout, err := Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	source, destination := filepath.Join(t.TempDir(), "source"), filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(source, "implement"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("skill body\n")
	if err := os.WriteFile(filepath.Join(source, "implement", "SKILL.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	spec := SkillBundleSpec{Version: "1.0.0", DestinationRoot: destination, ManagedSkills: []string{"implement"}, Files: []SkillBundleFile{{Path: "implement/SKILL.md", SHA256: hex.EncodeToString(sum[:])}}}
	installation := Installation{Layout: layout}
	if err := installation.InstallSkillBundle(source, spec); err != nil {
		t.Fatal(err)
	}
	verified, err := installation.VerifyRecordedSkillBundle(destination, "1.0.0", []string{"implement"})
	if err != nil || !verified {
		t.Fatalf("verified=%t err=%v", verified, err)
	}
	if err := os.WriteFile(filepath.Join(destination, "implement", "SKILL.md"), []byte("drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err = installation.VerifyRecordedSkillBundle(destination, "1.0.0", []string{"implement"})
	if err != nil || verified {
		t.Fatalf("drift verified=%t err=%v", verified, err)
	}
}
