//go:build darwin

package workflowhome

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func defaultWorkflowHomeRoot() (string, error) {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return "", errors.New("HOME is required to resolve Workflow Home")
	}
	return filepath.Join(home, "Library", "Application Support", DirectoryName), nil
}
