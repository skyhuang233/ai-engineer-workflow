//go:build !windows

package workflowhome

import (
	"path/filepath"
	"strings"
)

// SameFilesystemPath compares normalized paths on platforms without Windows
// short-name aliases. Existing symlinks are resolved when possible.
func SameFilesystemPath(left, right string) (bool, error) {
	left, _ = CanonicalFilesystemPath(left)
	right, _ = CanonicalFilesystemPath(right)
	return strings.EqualFold(left, right), nil
}

func CanonicalFilesystemPath(value string) (string, error) {
	value = filepath.Clean(value)
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return resolved, nil
	}
	return value, nil
}
