package startup

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func DatabaseIdentity(dsn string) (string, error) {
	path, memory, err := databasePath(dsn)
	if err != nil {
		return "", err
	}
	if memory {
		return "", nil
	}
	if runtime.GOOS == "windows" {
		path = normalizeWindowsNamespacePath(path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	canonical, err := resolveExistingPath(filepath.Clean(absolute))
	if err != nil {
		return "", fmt.Errorf("canonicalize database path: %w", err)
	}
	canonical = filepath.Clean(canonical)
	canonical, err = normalizeDatabasePathCase(canonical)
	if err != nil {
		return "", fmt.Errorf("normalize database path case: %w", err)
	}
	return canonical, nil
}

func normalizeWindowsNamespacePath(path string) string {
	path = filepath.FromSlash(path)
	for _, prefix := range []string{`\\?\UNC\`, `\\.\UNC\`, `\??\UNC\`} {
		if len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
			return `\\` + path[len(prefix):]
		}
	}
	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
			continue
		}
		remainder := path[len(prefix):]
		if len(remainder) >= 3 && remainder[1] == ':' && (remainder[2] == '\\' || remainder[2] == '/') {
			return remainder
		}
	}
	return path
}

func databasePath(dsn string) (string, bool, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", false, errors.New("database path is required")
	}
	if dsn == ":memory:" {
		return "", true, nil
	}
	if !strings.HasPrefix(strings.ToLower(dsn), "file:") {
		return dsn, false, nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", false, fmt.Errorf("parse SQLite file URI: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "file") {
		return "", false, errors.New("database URI must use the file scheme")
	}
	if strings.EqualFold(parsed.Query().Get("mode"), "memory") {
		return "", true, nil
	}
	path := parsed.Path
	if parsed.Opaque != "" {
		path, err = url.PathUnescape(parsed.Opaque)
		if err != nil {
			return "", false, fmt.Errorf("decode SQLite file URI: %w", err)
		}
	}
	if runtime.GOOS == "windows" && len(parsed.Host) == 2 && parsed.Host[1] == ':' {
		path = parsed.Host + "/" + strings.TrimLeft(path, "/")
	} else if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		path = "//" + parsed.Host + "/" + strings.TrimLeft(path, "/")
	}
	path = filepath.FromSlash(path)
	if runtime.GOOS == "windows" && len(path) >= 3 && (path[0] == '\\' || path[0] == '/') && path[2] == ':' {
		path = path[1:]
	}
	if path == "" || path == ":memory:" {
		return "", true, nil
	}
	return path, false, nil
}

func resolveExistingPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	resolvedParent, err := resolveExistingPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
