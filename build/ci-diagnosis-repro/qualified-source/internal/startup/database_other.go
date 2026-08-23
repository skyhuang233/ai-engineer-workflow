//go:build !windows

package startup

func validateLocalDatabasePath(string) error {
	return nil
}

func normalizeDatabasePathCase(path string) (string, error) {
	return path, nil
}
