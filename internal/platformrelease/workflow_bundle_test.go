package platformrelease

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestWorkflowSkillBundleAutomaticallyBindsPublishedPlanRoot(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	repository := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	bundle, err := os.ReadFile(filepath.Join(repository, "deploy", "platform", "skills", "agent-workflow", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(bundle)
	for _, required := range []string{"automatically", "workflow runtime-configure --source", "--root <plan-root-issue-number>", "not a user step", "codex doctor --json", "must not ask the user"} {
		if !strings.Contains(content, required) {
			t.Fatalf("Workflow Skill Bundle lacks automatic Plan Root binding contract %q", required)
		}
	}
	readme, err := os.ReadFile(filepath.Join(repository, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readme), "workflow runtime-configure") {
		t.Fatal("README exposes internal runtime configuration as a user setup step")
	}
}

func TestWorkflowSkillBundleUsesConciseOneLevelNavigationAndOperationalReferences(t *testing.T) {
	repository := workflowBundleRepositoryRoot(t)
	skillRoot := filepath.Join(repository, "deploy", "platform", "skills", "agent-workflow")
	main := readWorkflowBundleFile(t, filepath.Join(skillRoot, "SKILL.md"))
	if len(main) > 3000 {
		t.Fatalf("main SKILL.md is %d bytes; keep detailed operations in references", len(main))
	}
	references := map[string][]string{
		"plans.md":         {"workflow:plan", "workflow:active", "runtime-configure", "Plan Amendment"},
		"tickets.md":       {"workflow:ticket", "sub-issue", "dependencies", "independently reviewable"},
		"inbox.md":         {"workflow:inbox", "workflow-answer:<question-id>", "allowed answer", "uncertain"},
		"pull-requests.md": {"one persistent", "required checks", "human", "Review Feedback"},
		"authority.md":     {"Repository Admission", "Run Lease", "fencing", "cancellation"},
	}
	for name, required := range references {
		link := "references/" + name
		if !strings.Contains(main, link) {
			t.Fatalf("main SKILL.md does not navigate to %s", link)
		}
		content := readWorkflowBundleFile(t, filepath.Join(skillRoot, "references", name))
		if strings.Contains(content, "references/") {
			t.Fatalf("reference %s creates deeper reference navigation", name)
		}
		for _, phrase := range required {
			if !strings.Contains(content, phrase) {
				t.Fatalf("reference %s lacks operational contract %q", name, phrase)
			}
		}
	}
	for _, required := range []string{"AGENTS.md", ".workflow/repository.json", "docs/agents/issue-tracker.md", "docs/agents/domain.md", "Never bypass"} {
		if !strings.Contains(main, required) {
			t.Fatalf("main SKILL.md lacks routing/authority contract %q", required)
		}
	}
}

func TestWorkflowSkillBundleEverySourceFileIsManifestedInstalledAndReadBack(t *testing.T) {
	repository := workflowBundleRepositoryRoot(t)
	source := filepath.Join(repository, "deploy", "platform", "skills")
	files, err := collectPackageFiles(filepath.Join(t.TempDir(), "workflow.exe"), source)
	if err == nil {
		t.Fatal("collectPackageFiles unexpectedly accepted a missing CLI fixture")
	}
	cli := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(cli, []byte("workflow-cli"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err = collectPackageFiles(cli, source)
	if err != nil {
		t.Fatal(err)
	}
	var spec workflowhome.SkillBundleSpec
	spec.Version = "test"
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "workflow-home"))
	if err != nil {
		t.Fatal(err)
	}
	spec.DestinationRoot = filepath.Join(t.TempDir(), "installed")
	spec.ManagedSkills = []string{"agent-workflow"}
	want := []string{}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "agent-workflow/") {
			continue
		}
		sum := sha256.Sum256(file.Data)
		spec.Files = append(spec.Files, workflowhome.SkillBundleFile{Path: file.Path, SHA256: hex.EncodeToString(sum[:])})
		want = append(want, file.Path)
	}
	sort.Strings(want)
	if len(want) != 7 {
		t.Fatalf("manifested Workflow Skill Bundle files = %v, want SKILL.md, agents metadata, and five references", want)
	}
	installation := workflowhome.Installation{Layout: layout}
	if err := installation.InstallSkillBundle(source, spec); err != nil {
		t.Fatal(err)
	}
	verified, err := installation.VerifySkillBundle(spec)
	if err != nil || !verified {
		t.Fatalf("fresh installed bundle readback = %t, %v", verified, err)
	}
	for _, relative := range want {
		if _, err := os.Stat(filepath.Join(spec.DestinationRoot, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("installed bundle lacks %s: %v", relative, err)
		}
	}
}

func TestAssembledReleaseManifestIncludesEveryWorkflowSkillSourceFile(t *testing.T) {
	repository := workflowBundleRepositoryRoot(t)
	payload := t.TempDir()
	copyWorkflowBundleTree(t, filepath.Join(repository, "deploy", "platform", "skills"), filepath.Join(payload, "skills"))
	copyWorkflowBundleTree(t, filepath.Join(repository, "deploy", "platform", "repository-contract"), filepath.Join(payload, "repository-contract"))
	cli := filepath.Join(t.TempDir(), "workflow.exe")
	if err := os.WriteFile(cli, []byte("workflow-cli"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validManifest(fixtureArtifacts())
	manifest.Artifacts, manifest.BundledFiles, manifest.Provenance.Subjects = nil, nil, nil
	manifest.PlatformSetup.SkillBundle.ManagedSkills = []string{"agent-workflow"}
	assembly, err := Assemble(AssembleOptions{OutputDirectory: t.TempDir(), WorkflowExecutable: cli, PayloadDirectory: payload, Manifest: manifest, SigningKey: key})
	if err != nil {
		t.Fatal(err)
	}
	manifested := make(map[string]string, len(assembly.Manifest.BundledFiles))
	for _, file := range assembly.Manifest.BundledFiles {
		manifested[file.Path] = file.SHA256
	}
	sourceRoot := filepath.Join(repository, "deploy", "platform", "skills", "agent-workflow")
	err = filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, relErr := filepath.Rel(filepath.Join(repository, "deploy", "platform"), path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		manifestPath := filepath.ToSlash(relative)
		if manifested[manifestPath] != hex.EncodeToString(sum[:]) {
			t.Fatalf("assembled manifest omits or mis-digests %s", manifestPath)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowSkillBundleForwardFixturesContainOnlyRawUserPrompts(t *testing.T) {
	repository := workflowBundleRepositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(repository, "internal", "platformrelease", "testdata", "workflow-skill-forward-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prompts []struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prompts); err != nil {
		t.Fatal(err)
	}
	if len(prompts) < 3 {
		t.Fatalf("forward prompt count = %d, want independent plan, Inbox, and pull-request tasks", len(prompts))
	}
	for _, fixture := range prompts {
		if strings.TrimSpace(fixture.Name) == "" || !strings.Contains(fixture.Prompt, "Use $agent-workflow") {
			t.Fatalf("forward fixture is not a raw skill invocation: %#v", fixture)
		}
		for _, leaked := range []string{"expected output", "must mention", "assert", "oracle", "the correct answer"} {
			if strings.Contains(strings.ToLower(fixture.Prompt), leaked) {
				t.Fatalf("forward fixture %q leaks an expected-answer hint %q", fixture.Name, leaked)
			}
		}
	}
}

func workflowBundleRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}

func readWorkflowBundleFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func copyWorkflowBundleTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
