//go:build !windows

package platformrelease

import "os"

func securePrivateKey(path string) error { return os.Chmod(path, 0o600) }
