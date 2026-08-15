// Package repositorycontract renders and verifies the exact repository-side
// Agent Workflow contract.
package repositorycontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	ManifestPath             = ".workflow/repository.json"
	BlockStart               = "<!-- agent-workflow:start -->"
	BlockEnd                 = "<!-- agent-workflow:end -->"
	RequiredCheckName        = "workflow-contract"
	GitHubActionsAppID int64 = 15368
)

type Manifest struct {
	SchemaVersion      int           `json:"schema_version"`
	ContractVersion    string        `json:"contract_version"`
	Repository         string        `json:"repository"`
	DefaultBranch      string        `json:"default_branch"`
	IssueTracker       string        `json:"issue_tracker"`
	DomainLayout       string        `json:"domain_layout"`
	RequiredCheck      string        `json:"required_check"`
	ManagedBlockSHA256 string        `json:"managed_block_sha256"`
	ManagedFiles       []ManagedFile `json:"managed_files"`
}
type ManagedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func Render(domainLayout string, existingAgents []byte, repository, defaultBranch string) (map[string][]byte, Manifest, string, error) {
	if repository == "" || defaultBranch == "" {
		return nil, Manifest{}, "", errors.New("repository identity and default branch are required")
	}
	if domainLayout == "" {
		domainLayout = "single-context"
	}
	if domainLayout != "single-context" && domainLayout != "multi-context" {
		return nil, Manifest{}, "", errors.New("domain layout must be single-context or multi-context")
	}
	agents, err := reconcileBlock(existingAgents)
	if err != nil {
		return nil, Manifest{}, "", err
	}
	files := map[string][]byte{"AGENTS.md": agents}
	whole := make(map[string][]byte, len(wholeFiles))
	for path, data := range wholeFiles {
		whole[path] = append([]byte(nil), data...)
	}
	if domainLayout == "multi-context" {
		whole["docs/agents/domain.md"] = append([]byte(nil), multiContextDomain...)
	}
	paths := make([]string, 0, len(wholeFiles))
	for path, data := range whole {
		copyData := append([]byte(nil), data...)
		if path == ".github/workflows/workflow-contract.yml" {
			copyData = bytes.ReplaceAll(copyData, []byte("branches: [main]"), []byte("branches: ["+defaultBranch+"]"))
		}
		files[path] = copyData
		paths = append(paths, path)
	}
	sort.Strings(paths)
	manifest := Manifest{SchemaVersion: 1, ContractVersion: "1", Repository: repository, DefaultBranch: defaultBranch, IssueTracker: "github-issues", DomainLayout: domainLayout, RequiredCheck: RequiredCheckName, ManagedBlockSHA256: digest(block)}
	for _, path := range paths {
		manifest.ManagedFiles = append(manifest.ManagedFiles, ManagedFile{Path: path, SHA256: digest(files[path])})
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, Manifest{}, "", err
	}
	encoded = append(encoded, '\n')
	files[ManifestPath] = encoded
	return files, manifest, digest(encoded), nil
}
func Write(root string, files map[string][]byte) error {
	for path, data := range files {
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
func Verify(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ManifestPath)))
	if err != nil {
		return "", err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	if manifest.SchemaVersion != 1 || manifest.ContractVersion != "1" || manifest.RequiredCheck != RequiredCheckName || manifest.DomainLayout != "single-context" && manifest.DomainLayout != "multi-context" {
		return "", errors.New("unsupported Repository Contract Manifest")
	}
	for _, managed := range manifest.ManagedFiles {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(managed.Path)))
		if err != nil {
			return "", err
		}
		if digest(content) != managed.SHA256 {
			return "", fmt.Errorf("managed file %q drifted", managed.Path)
		}
	}
	agents, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		return "", err
	}
	managedBlock, ok := extractBlock(agents)
	if !ok || digest(managedBlock) != manifest.ManagedBlockSHA256 {
		return "", errors.New("AGENTS.md managed block drifted")
	}
	return digest(data), nil
}

// VerifyRemote verifies only platform-owned files and the managed AGENTS.md
// block. User-owned bytes outside that block remain outside admission authority.
func VerifyRemote(fetch func(string) ([]byte, error), repository, defaultBranch, expectedDigest string) (Manifest, error) {
	data, err := fetch(ManifestPath)
	if err != nil {
		return Manifest{}, err
	}
	if digest(data) != expectedDigest {
		return Manifest{}, errors.New("Repository Contract Manifest digest drifted")
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.SchemaVersion != 1 || manifest.ContractVersion != "1" || manifest.Repository != repository || manifest.DefaultBranch != defaultBranch || manifest.RequiredCheck != RequiredCheckName || manifest.DomainLayout != "single-context" && manifest.DomainLayout != "multi-context" {
		return Manifest{}, errors.New("Repository Contract Manifest identity is invalid")
	}
	for _, managed := range manifest.ManagedFiles {
		content, err := fetch(managed.Path)
		if err != nil {
			return Manifest{}, err
		}
		if digest(content) != managed.SHA256 {
			return Manifest{}, fmt.Errorf("managed file %q drifted", managed.Path)
		}
	}
	agents, err := fetch("AGENTS.md")
	if err != nil {
		return Manifest{}, err
	}
	managedBlock, ok := extractBlock(agents)
	if !ok || digest(managedBlock) != manifest.ManagedBlockSHA256 {
		return Manifest{}, errors.New("AGENTS.md managed block drifted")
	}
	return manifest, nil
}
func reconcileBlock(existing []byte) ([]byte, error) {
	start := bytes.Index(existing, []byte(BlockStart))
	end := bytes.Index(existing, []byte(BlockEnd))
	if start < 0 && end < 0 {
		result := append([]byte(nil), existing...)
		if len(result) > 0 && !bytes.HasSuffix(result, []byte("\n")) {
			result = append(result, '\n')
		}
		if len(result) > 0 {
			result = append(result, '\n')
		}
		return append(result, block...), nil
	}
	if start < 0 || end < start {
		return nil, errors.New("AGENTS.md has malformed Workflow-managed block")
	}
	end += len(BlockEnd)
	if end < len(existing) && existing[end] == '\r' {
		end++
	}
	if end < len(existing) && existing[end] == '\n' {
		end++
	}
	result := append([]byte(nil), existing[:start]...)
	result = append(result, block...)
	result = append(result, existing[end:]...)
	return result, nil
}
func extractBlock(value []byte) ([]byte, bool) {
	if bytes.Count(value, []byte(BlockStart)) != 1 || bytes.Count(value, []byte(BlockEnd)) != 1 {
		return nil, false
	}
	start := bytes.Index(value, []byte(BlockStart))
	if start < 0 {
		return nil, false
	}
	endRelative := bytes.Index(value[start:], []byte(BlockEnd))
	if endRelative < 0 {
		return nil, false
	}
	end := start + endRelative + len(BlockEnd)
	if end < len(value) && value[end] == '\r' {
		end++
	}
	if end < len(value) && value[end] == '\n' {
		end++
	}
	return value[start:end], true
}
func digest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
