package workflowhome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	SkillOwnershipFile       = ".agent-workflow-owner.json"
	SkillBundleOwnershipFile = "workflow-skills.owner.json"
)

type SkillBundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type SkillBundleSpec struct {
	Version         string            `json:"version"`
	DestinationRoot string            `json:"destination_root"`
	ManagedSkills   []string          `json:"managed_skills"`
	Files           []SkillBundleFile `json:"files"`
}

type skillBundleOwnership struct {
	Owner       string            `json:"owner"`
	Version     string            `json:"version"`
	Skills      []string          `json:"skills"`
	FileDigests map[string]string `json:"file_digests"`
	InstalledAt time.Time         `json:"installed_at"`
}

// VerifyRecordedSkillBundle reconstructs the exact installed digest contract
// from platform-owned state, then verifies every declared user-level skill and
// file. The caller still supplies the release-declared version and skill names.
func (i Installation) VerifyRecordedSkillBundle(destinationRoot, version string, managedSkills []string) (bool, error) {
	state, err := os.ReadFile(filepath.Join(i.Layout.Config, SkillBundleOwnershipFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var active skillBundleOwnership
	if json.Unmarshal(state, &active) != nil || active.Owner != ownerID {
		return false, errors.New("Workflow Skill Bundle ownership state is invalid")
	}
	if active.Version != version || !equalStringSlices(active.Skills, managedSkills) {
		return false, nil
	}
	files := make([]SkillBundleFile, 0, len(active.FileDigests))
	for path, digest := range active.FileDigests {
		files = append(files, SkillBundleFile{Path: path, SHA256: digest})
	}
	return i.VerifySkillBundle(SkillBundleSpec{Version: version, DestinationRoot: destinationRoot, ManagedSkills: managedSkills, Files: files})
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a, b := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// VerifySkillBundle reads the active user-level bundle through the same exact
// version and digest contract used for installation. A missing or older owned
// bundle returns false; an unowned same-name directory fails closed.
func (i Installation) VerifySkillBundle(spec SkillBundleSpec) (bool, error) {
	expected, err := expectedSkillBundle(spec)
	if err != nil {
		return false, err
	}
	state, err := os.ReadFile(filepath.Join(i.Layout.Config, SkillBundleOwnershipFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var active skillBundleOwnership
	if json.Unmarshal(state, &active) != nil || active.Owner != ownerID {
		return false, errors.New("Workflow Skill Bundle ownership state is invalid")
	}
	if active.Version != spec.Version || !equalDigestMaps(active.FileDigests, expected) {
		return false, nil
	}
	for _, skill := range spec.ManagedSkills {
		target := filepath.Join(spec.DestinationRoot, skill)
		info, statErr := os.Lstat(target)
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		if statErr != nil {
			return false, statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("existing skill %q is not owned by Agent Workflow", skill)
		}
		data, readErr := os.ReadFile(filepath.Join(target, SkillOwnershipFile))
		var record skillBundleOwnership
		if readErr != nil || json.Unmarshal(data, &record) != nil || record.Owner != ownerID {
			return false, fmt.Errorf("existing skill %q is not owned by Agent Workflow", skill)
		}
		if record.Version != spec.Version || !equalDigestMaps(record.FileDigests, expected) {
			return false, nil
		}
	}
	actual := make(map[string]string, len(expected))
	for _, skill := range spec.ManagedSkills {
		root := filepath.Join(spec.DestinationRoot, skill)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root || entry.IsDir() {
				return nil
			}
			if entry.Name() == SkillOwnershipFile && filepath.Dir(path) == root {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return fmt.Errorf("installed Workflow skill path %q is not a regular file", path)
			}
			relative, err := filepath.Rel(spec.DestinationRoot, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			actual[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	return equalDigestMaps(actual, expected), nil
}

// InstallSkillBundle verifies the exact declared source tree before touching
// Codex's user skill root. It preflights every name, stages all replacements on
// the destination volume, rolls the entire set back on a failed switch, and
// publishes the bundle ownership record only after every skill is active.
func (i Installation) InstallSkillBundle(sourceRoot string, spec SkillBundleSpec) error {
	validated, err := validateSkillBundle(sourceRoot, spec)
	if err != nil {
		return err
	}
	if err := i.Layout.Ensure(); err != nil {
		return err
	}
	if err := os.MkdirAll(spec.DestinationRoot, 0o700); err != nil {
		return fmt.Errorf("create Codex user skill root: %w", err)
	}
	for _, skill := range spec.ManagedSkills {
		target := filepath.Join(spec.DestinationRoot, skill)
		info, statErr := os.Lstat(target)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedSkill(target) {
			return fmt.Errorf("existing skill %q is not owned by Agent Workflow", skill)
		}
	}

	stageRoot, err := os.MkdirTemp(spec.DestinationRoot, ".agent-workflow-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageRoot)
	for _, file := range validated.files {
		destination := filepath.Join(stageRoot, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := atomicCopy(filepath.Join(sourceRoot, filepath.FromSlash(file.Path)), destination, 0o600); err != nil {
			return fmt.Errorf("stage Workflow Skill Bundle file %q: %w", file.Path, err)
		}
	}
	record := skillBundleOwnership{Owner: ownerID, Version: spec.Version, Skills: append([]string(nil), spec.ManagedSkills...), FileDigests: validated.digests, InstalledAt: time.Now().UTC()}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return err
	}
	for _, skill := range spec.ManagedSkills {
		if err := atomicWrite(filepath.Join(stageRoot, skill, SkillOwnershipFile), append(recordBytes, '\n'), 0o600); err != nil {
			return err
		}
	}

	backupRoot, err := os.MkdirTemp(spec.DestinationRoot, ".agent-workflow-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(backupRoot)
	type switchedSkill struct {
		name        string
		hadPrevious bool
	}
	var switched []switchedSkill
	rollback := func() {
		for index := len(switched) - 1; index >= 0; index-- {
			item := switched[index]
			target := filepath.Join(spec.DestinationRoot, item.name)
			_ = os.RemoveAll(target)
			if item.hadPrevious {
				_ = os.Rename(filepath.Join(backupRoot, item.name), target)
			}
		}
	}
	for _, skill := range spec.ManagedSkills {
		target := filepath.Join(spec.DestinationRoot, skill)
		_, statErr := os.Stat(target)
		hadPrevious := statErr == nil
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			rollback()
			return statErr
		}
		if hadPrevious {
			if err := os.Rename(target, filepath.Join(backupRoot, skill)); err != nil {
				rollback()
				return fmt.Errorf("archive owned skill %q: %w", skill, err)
			}
		}
		if err := os.Rename(filepath.Join(stageRoot, skill), target); err != nil {
			if hadPrevious {
				_ = os.Rename(filepath.Join(backupRoot, skill), target)
			}
			rollback()
			return fmt.Errorf("activate Workflow skill %q: %w", skill, err)
		}
		switched = append(switched, switchedSkill{name: skill, hadPrevious: hadPrevious})
	}
	if err := atomicWrite(filepath.Join(i.Layout.Config, SkillBundleOwnershipFile), append(recordBytes, '\n'), 0o600); err != nil {
		rollback()
		return fmt.Errorf("publish Workflow Skill Bundle ownership: %w", err)
	}
	return nil
}

type validatedSkillBundle struct {
	files   []SkillBundleFile
	digests map[string]string
}

func validateSkillBundle(sourceRoot string, spec SkillBundleSpec) (validatedSkillBundle, error) {
	if !filepath.IsAbs(sourceRoot) {
		return validatedSkillBundle{}, errors.New("exact Workflow Skill Bundle source, version, destination, skills, and files are required")
	}
	expected, err := expectedSkillBundle(spec)
	if err != nil {
		return validatedSkillBundle{}, err
	}
	skills := make(map[string]struct{}, len(spec.ManagedSkills))
	for _, skill := range spec.ManagedSkills {
		skills[skill] = struct{}{}
	}
	actual := make(map[string]string, len(expected))
	err = filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceRoot || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("Workflow Skill Bundle source %q is not a regular file", path)
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		actual[relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return validatedSkillBundle{}, err
	}
	if !equalDigestMaps(actual, expected) {
		if len(actual) != len(expected) {
			return validatedSkillBundle{}, errors.New("Workflow Skill Bundle source file set does not exactly match the release manifest")
		}
		for path, digest := range expected {
			if actual[path] != digest {
				return validatedSkillBundle{}, fmt.Errorf("Workflow Skill Bundle file %q checksum mismatch", path)
			}
		}
	}
	files := append([]SkillBundleFile(nil), spec.Files...)
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return validatedSkillBundle{files: files, digests: expected}, nil
}

func expectedSkillBundle(spec SkillBundleSpec) (map[string]string, error) {
	if !filepath.IsAbs(spec.DestinationRoot) || strings.TrimSpace(spec.Version) == "" || len(spec.ManagedSkills) == 0 || len(spec.Files) == 0 {
		return nil, errors.New("exact Workflow Skill Bundle version, destination, skills, and files are required")
	}
	skills := make(map[string]struct{}, len(spec.ManagedSkills))
	for _, skill := range spec.ManagedSkills {
		if filepath.Base(skill) != skill || skill == "." || skill == ".." || strings.TrimSpace(skill) == "" {
			return nil, fmt.Errorf("managed skill name %q is invalid", skill)
		}
		if _, exists := skills[skill]; exists {
			return nil, fmt.Errorf("managed skill %q is duplicated", skill)
		}
		skills[skill] = struct{}{}
	}
	expected := make(map[string]string, len(spec.Files))
	for _, file := range spec.Files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
		parts := strings.Split(clean, "/")
		digest := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(file.SHA256), "sha256:"))
		if clean != file.Path || len(parts) < 2 || len(digest) != 64 {
			return nil, fmt.Errorf("Workflow Skill Bundle file %q is invalid", file.Path)
		}
		if _, managed := skills[parts[0]]; !managed {
			return nil, fmt.Errorf("Workflow Skill Bundle file %q is outside a managed skill", file.Path)
		}
		if _, duplicate := expected[file.Path]; duplicate {
			return nil, fmt.Errorf("Workflow Skill Bundle file %q is duplicated", file.Path)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("Workflow Skill Bundle file %q checksum is invalid", file.Path)
		}
		expected[file.Path] = digest
	}
	for skill := range skills {
		found := false
		for file := range expected {
			if strings.HasPrefix(file, skill+"/") {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("managed skill %q has no declared files", skill)
		}
	}
	return expected, nil
}

func equalDigestMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for path, digest := range left {
		if right[path] != digest {
			return false
		}
	}
	return true
}

func ownedSkill(path string) bool {
	data, err := os.ReadFile(filepath.Join(path, SkillOwnershipFile))
	if err != nil {
		return false
	}
	var record skillBundleOwnership
	return json.Unmarshal(data, &record) == nil && record.Owner == ownerID && strings.TrimSpace(record.Version) != ""
}
