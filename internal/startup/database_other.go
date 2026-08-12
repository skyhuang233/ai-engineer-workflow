//go:build !windows

package startup

func normalizeDatabasePathCase(path string) (string, error) {
	return path, nil
}
