//go:build !windows

package credential

import "os"

func replaceCredentialFile(source, destination string) error { return os.Rename(source, destination) }
