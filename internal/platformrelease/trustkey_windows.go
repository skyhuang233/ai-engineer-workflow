//go:build windows

package platformrelease

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func securePrivateKey(path string) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.Clean(path), `\\`) {
		return errors.New("offline private key must use an absolute local path")
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("offline private key must not be a reparse point")
	}
	user := strings.TrimSpace(os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME"))
	if user == `\` {
		return errors.New("current Windows user identity is unavailable")
	}
	command := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", user+":F", "*S-1-5-18:F", "*S-1-5-32-544:F")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("restrict offline private key permissions: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
