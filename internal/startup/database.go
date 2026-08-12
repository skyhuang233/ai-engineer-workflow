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
	canonical, err := DatabaseFilePath(dsn)
	if err != nil || canonical == "" {
		return canonical, err
	}
	canonical, err = normalizeDatabasePathCase(canonical)
	if err != nil {
		return "", fmt.Errorf("normalize database path case: %w", err)
	}
	return canonical, nil
}

func DatabaseFilePath(dsn string) (string, error) {
	path, memory, err := databasePath(dsn)
	if err != nil {
		return "", err
	}
	if memory {
		return "", nil
	}
	if runtime.GOOS == "windows" {
		path, err = normalizeWindowsNamespacePath(path)
		if err != nil {
			return "", fmt.Errorf("normalize Windows database path: %w", err)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	canonical, err := resolveExistingPath(filepath.Clean(absolute))
	if err != nil {
		return "", fmt.Errorf("canonicalize database path: %w", err)
	}
	return filepath.Clean(canonical), nil
}

func normalizeWindowsNamespacePath(path string) (string, error) {
	path = filepath.FromSlash(path)
	for _, prefix := range []string{`\\?\UNC\`, `\\.\UNC\`, `\??\UNC\`} {
		if len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
			remainder := path[len(prefix):]
			if err := validateWindowsNamespaceComponents(remainder, 2); err != nil {
				return "", err
			}
			return `\\` + remainder, nil
		}
	}
	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
			continue
		}
		remainder := path[len(prefix):]
		if len(remainder) >= 3 && remainder[1] == ':' && (remainder[2] == '\\' || remainder[2] == '/') {
			if err := validateWindowsNamespaceComponents(remainder, 1); err != nil {
				return "", err
			}
			return remainder, nil
		}
		if strings.HasPrefix(strings.ToLower(remainder), `volume{`) {
			if err := validateWindowsNamespaceComponents(remainder, 1); err != nil {
				return "", err
			}
			return `\\?\` + remainder, nil
		}
		return "", fmt.Errorf("unsupported Windows device namespace %q", path[:len(prefix)]+strings.SplitN(remainder, `\`, 2)[0])
	}
	return path, nil
}

func validateWindowsNamespaceComponents(path string, rootComponents int) error {
	components := strings.Split(path, `\`)
	if len(components) <= rootComponents {
		return errors.New("Windows namespace database path is incomplete")
	}
	for index, component := range components {
		changesParsing := component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ")
		if changesParsing || index >= rootComponents && windowsReservedPathComponent(component) {
			return fmt.Errorf("unsupported Windows namespace path component %q", component)
		}
	}
	return nil
}

func windowsReservedPathComponent(component string) bool {
	base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func databasePath(dsn string) (string, bool, error) {
	if runtime.GOOS == "windows" && isWindowsNamespacePath(dsn) {
		return dsn, false, nil
	}
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

func isWindowsNamespacePath(path string) bool {
	path = filepath.FromSlash(path)
	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
			return true
		}
	}
	return false
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
