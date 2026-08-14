//go:build !windows

package workflowhome

import "os"

func replaceFile(source, destination string) error { return os.Rename(source, destination) }
