package workflowhome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ExecutableName = "workflow.exe"
	OwnershipFile  = "workflow.owner.json"
	ownerID        = "agent-workflow-platform"
)

type Installation struct{ Layout Layout }

type ownership struct {
	Owner        string    `json:"owner"`
	Version      string    `json:"version"`
	SourceSHA256 string    `json:"source_sha256"`
	InstalledAt  time.Time `json:"installed_at"`
}

// VerifyVersion reads back both the platform-owned installation record and the
// executable bytes. A matching version label alone is never sufficient.
func (i Installation) VerifyVersion(version, expectedSHA256 string) (bool, error) {
	executable := filepath.Join(i.Layout.Bin, ExecutableName)
	recordPath := filepath.Join(i.Layout.Bin, OwnershipFile)
	recordRaw, err := os.ReadFile(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record ownership
	if json.Unmarshal(recordRaw, &record) != nil || record.Owner != ownerID {
		return false, errors.New("existing workflow executable is not owned by Agent Workflow")
	}
	data, err := os.ReadFile(executable)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	wanted := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expectedSHA256)), "sha256:")
	return record.Version == strings.TrimSpace(version) && record.SourceSHA256 == actual && wanted != "" && wanted == actual, nil
}

func ReconcilePath(current, bin string) string {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return current
	}
	parts, output, found := filepath.SplitList(current), []string{}, false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(part), filepath.Clean(bin)) {
			if found {
				continue
			}
			found = true
		}
		output = append(output, part)
	}
	if !found {
		output = append(output, bin)
	}
	return strings.Join(output, string(os.PathListSeparator))
}

func (i Installation) InstallVersion(version, source, expectedSHA256 string) error {
	version = strings.TrimSpace(version)
	if version == "" || !filepath.IsAbs(source) {
		return errors.New("version and absolute source executable are required")
	}
	if err := i.Layout.Ensure(); err != nil {
		return err
	}
	currentExecutable := filepath.Join(i.Layout.Bin, ExecutableName)
	ownerPath := filepath.Join(i.Layout.Bin, OwnershipFile)
	if _, err := os.Stat(currentExecutable); err == nil {
		data, readErr := os.ReadFile(ownerPath)
		var existing ownership
		if readErr != nil || json.Unmarshal(data, &existing) != nil || existing.Owner != ownerID {
			return errors.New("existing workflow executable is not owned by Agent Workflow")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read workflow executable: %w", err)
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	wanted := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expectedSHA256)), "sha256:")
	if wanted == "" || wanted != actual {
		return errors.New("workflow executable checksum mismatch")
	}
	versionDir := filepath.Join(i.Layout.Versions, version)
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		return err
	}
	versionExecutable := filepath.Join(versionDir, ExecutableName)
	if err := atomicCopy(source, versionExecutable, 0o700); err != nil {
		return err
	}
	if err := atomicCopy(versionExecutable, currentExecutable, 0o700); err != nil {
		return err
	}
	record := ownership{Owner: ownerID, Version: version, SourceSHA256: actual, InstalledAt: time.Now().UTC()}
	recordBytes, _ := json.Marshal(record)
	return atomicWrite(ownerPath, append(recordBytes, '\n'), 0o600)
}

func atomicCopy(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".install-*.tmp")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(path, destination)
}

func atomicWrite(destination string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".state-*.tmp")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(path, destination)
}
