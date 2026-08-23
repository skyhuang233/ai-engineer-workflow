//go:build !windows

package controlplane

import "os"

func replaceRuntimeFile(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return os.Chmod(destination, 0o600)
}
