//go:build windows

package workflowhome

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// SameFilesystemPath treats equivalent Windows 8.3 and long spellings of an
// existing path as the same identity. A planned non-existent path retains its
// normalized spelling.
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
	input, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return "", err
	}
	for size := uint32(260); size <= 32768; size *= 2 {
		buffer := make([]uint16, size)
		length, callErr := windows.GetLongPathName(input, &buffer[0], uint32(len(buffer)))
		if callErr != nil || length == 0 {
			break
		}
		if length < uint32(len(buffer)) {
			return filepath.Clean(windows.UTF16ToString(buffer[:length])), nil
		}
	}
	return value, nil
}
