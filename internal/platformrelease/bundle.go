package platformrelease

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	workerImagePattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9_.-]+/[a-z0-9_./-]+@sha256:[0-9a-f]{64}$`)
)

func ValidatePlatformVersion(value string) error {
	parts := versionPattern.FindStringSubmatch(value)
	if parts == nil {
		return errors.New("version must be a bare semantic version core")
	}
	for _, part := range parts[1:] {
		if n, err := strconv.ParseUint(part, 10, 32); err != nil || n > 2147483647 {
			return errors.New("version components must fit signed 32-bit range")
		}
	}
	return nil
}

// BundleManifest is the authenticated inner contract of the one release
// asset. The GitHub Asset SHA-256 authenticates this root manifest.
type BundleManifest struct {
	SchemaVersion        int           `json:"schema_version"`
	SetupProtocolVersion int           `json:"setup_protocol_version"`
	Version              string        `json:"version"`
	Compatibility        Compatibility `json:"compatibility"`
	Files                []BundleFile  `json:"files"`
}

type Compatibility struct {
	OS                    string `json:"os"`
	Architecture          string `json:"architecture"`
	DatabaseSchema        int    `json:"database_schema"`
	WorkerImage           string `json:"worker_image"`
	DockerDesktopVersion  string `json:"docker_desktop_version"`
	DockerInstallerURL    string `json:"docker_installer_url"`
	DockerInstallerSHA256 string `json:"docker_installer_sha256"`
}

type BundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (m BundleManifest) Validate() error {
	if m.SchemaVersion != 1 || m.SetupProtocolVersion != 1 {
		return errors.New("unsupported Windows Bundle manifest schema")
	}
	if err := ValidatePlatformVersion(m.Version); err != nil {
		return err
	}
	if m.Compatibility.OS != "windows" || m.Compatibility.Architecture != "amd64" || m.Compatibility.DatabaseSchema <= 0 || !workerImagePattern.MatchString(m.Compatibility.WorkerImage) || strings.TrimSpace(m.Compatibility.DockerDesktopVersion) == "" || !strings.HasPrefix(m.Compatibility.DockerInstallerURL, "https://") || !sha256Pattern.MatchString(m.Compatibility.DockerInstallerSHA256) {
		return errors.New("Windows Bundle compatibility contract is incomplete")
	}
	if len(m.Files) < 4 {
		return errors.New("Windows Bundle inventory is incomplete")
	}
	required := map[string]bool{"setup/workflow-setup.exe": false, "platform/workflow.exe": false}
	seen := map[string]bool{}
	for _, file := range m.Files {
		if !safeBundlePath(file.Path) || seen[file.Path] || file.Size <= 0 || !sha256Pattern.MatchString(file.SHA256) {
			return fmt.Errorf("invalid Bundle inventory entry %q", file.Path)
		}
		seen[file.Path] = true
		if _, ok := required[file.Path]; ok {
			required[file.Path] = true
		}
	}
	for name, present := range required {
		if !present {
			return fmt.Errorf("Windows Bundle lacks %s", name)
		}
	}
	return nil
}

func (m BundleManifest) Canonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

type BundleAssembleOptions struct {
	Output             string
	SetupExecutable    string
	WorkflowExecutable string
	PayloadDirectory   string
	Manifest           BundleManifest
}

// AssembleBundle writes exactly workflow-windows-amd64.zip. There are no
// sibling functional assets and the manifest lives at the archive root.
func AssembleBundle(options BundleAssembleOptions) error {
	files := map[string][]byte{}
	for source, name := range map[string]string{options.SetupExecutable: "setup/workflow-setup.exe", options.WorkflowExecutable: "platform/workflow.exe"} {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		files[name] = data
	}
	err := filepath.WalkDir(options.PayloadDirectory, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("Bundle payload must contain only regular files")
		}
		relative, err := filepath.Rel(options.PayloadDirectory, filePath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !safeBundlePath(name) || name == "platform-release.json" || strings.HasPrefix(name, "setup/") || strings.HasPrefix(name, "platform/") {
			return fmt.Errorf("invalid Bundle payload path %q", name)
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("duplicate Bundle entry %q", name)
		}
		files[name] = data
		return nil
	})
	if err != nil {
		return err
	}
	manifest := options.Manifest
	manifest.Files = manifest.Files[:0]
	for name, data := range files {
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, BundleFile{Path: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	raw, err := manifest.Canonical()
	if err != nil {
		return err
	}
	files["platform-release.json"] = raw
	archive, err := deterministicBundleZip(files)
	if err != nil {
		return err
	}
	if filepath.Base(options.Output) != "workflow-windows-amd64.zip" {
		return errors.New("Bundle output must be workflow-windows-amd64.zip")
	}
	if err := os.MkdirAll(filepath.Dir(options.Output), 0o755); err != nil {
		return err
	}
	return os.WriteFile(options.Output, archive, 0o644)
}

func deterministicBundleZip(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	fixed := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetModTime(fixed)
		h.SetMode(0o644)
		out, err := w.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := out.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func safeBundlePath(name string) bool {
	cleaned := path.Clean(name)
	return name != "platform-release.json" && name != "" && cleaned == name && !strings.HasPrefix(name, "../") && !strings.HasPrefix(name, "/") && !strings.Contains(name, "\\")
}
