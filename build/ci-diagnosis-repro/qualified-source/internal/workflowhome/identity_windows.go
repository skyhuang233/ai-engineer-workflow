//go:build windows

package workflowhome

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// SameFilesystemPath treats equivalent Windows 8.3 and long spellings as the
// same identity, including a planned path below an existing aliased ancestor.
func SameFilesystemPath(left, right string) (bool, error) {
	left, err := CanonicalFilesystemPath(left)
	if err != nil {
		return false, err
	}
	right, err = CanonicalFilesystemPath(right)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(left, right), nil
}

func CanonicalFilesystemPath(value string) (string, error) {
	value = filepath.Clean(value)
	candidate := value
	var suffix []string
	for {
		long, found, err := longPathName(candidate)
		if err != nil {
			return "", err
		}
		if found {
			return filepath.Clean(filepath.Join(append([]string{long}, suffix...)...)), nil
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return value, nil
		}
		suffix = append([]string{filepath.Base(candidate)}, suffix...)
		candidate = parent
	}
}

func longPathName(value string) (string, bool, error) {
	input, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return "", false, err
	}
	for size := uint32(260); size <= 32768; size *= 2 {
		buffer := make([]uint16, size)
		length, callErr := windows.GetLongPathName(input, &buffer[0], uint32(len(buffer)))
		if callErr != nil || length == 0 {
			return "", false, nil
		}
		if length < uint32(len(buffer)) {
			return filepath.Clean(windows.UTF16ToString(buffer[:length])), true, nil
		}
	}
	return "", false, nil
}
