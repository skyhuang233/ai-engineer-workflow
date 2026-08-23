//go:build windows

package workflowhome

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// SecureCredentialPath rejects reparse-point traversal and, when repair is
// requested, replaces inherited access with explicit current-user, SYSTEM,
// and local Administrators full-control entries. The PAT body is never read.
func SecureCredentialPath(path string, repair bool) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.Clean(path), `\\`) {
		return errors.New("credential path must be absolute and local")
	}
	if err := rejectReparseTraversal(path); err != nil {
		return err
	}
	target := path
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		target = filepath.Dir(target)
	} else if err != nil {
		return err
	}
	if !repair {
		return nil
	}
	user := strings.TrimSpace(os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME"))
	if user == `\` {
		return errors.New("current Windows user identity is unavailable")
	}
	command := exec.Command("icacls.exe", target, "/inheritance:r", "/grant:r", user+":(OI)(CI)F", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("restrict Workflow Home permissions: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func rejectReparseTraversal(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	relative := strings.TrimPrefix(clean, volume)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(relative, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(pointer)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			break
		}
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("credential path traverses reparse point %q", current)
		}
	}
	return nil
}
