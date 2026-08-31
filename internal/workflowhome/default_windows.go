//go:build windows

package workflowhome

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func defaultWorkflowHomeRoot() (string, error) {
	localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if localAppData == "" {
		return "", errors.New("LOCALAPPDATA is required to resolve Workflow Home")
	}
	return filepath.Join(localAppData, DirectoryName), nil
}
