//go:build windows

package startup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func normalizeDatabasePathCase(path string) (string, error) {
	path = normalizeWindowsVolumeCase(path)
	caseSensitive, known, err := windowsDirectoryCaseSensitivity(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !known || caseSensitive {
		return path, nil
	}
	return filepath.Join(filepath.Dir(path), strings.ToLower(filepath.Base(path))), nil
}

func normalizeWindowsVolumeCase(path string) string {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return path
	}
	return strings.ToLower(volume) + path[len(volume):]
}

func windowsDirectoryCaseSensitivity(path string) (bool, bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, err
	}
	defer directory.Close()

	var info struct {
		flags uint32
	}
	err = windows.GetFileInformationByHandleEx(
		windows.Handle(directory.Fd()),
		windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
			errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			return false, false, nil
		}
		return false, false, err
	}
	return info.flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, true, nil
}
