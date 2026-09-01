// Package workflowhome owns the user-scoped Agent Workflow installation layout.
package workflowhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DirectoryName = "AgentWorkflow"

type Layout struct {
	Root           string
	Bin            string
	Versions       string
	Config         string
	State          string
	Workspaces     string
	Backups        string
	Logs           string
	CredentialFile string
}

func Resolve(override string) (Layout, error) {
	root := strings.TrimSpace(override)
	if root == "" {
		var err error
		root, err = defaultWorkflowHomeRoot()
		if err != nil {
			return Layout{}, err
		}
	}
	if !filepath.IsAbs(root) {
		return Layout{}, errors.New("Workflow Home must be an absolute local path")
	}
	clean, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return Layout{}, fmt.Errorf("resolve Workflow Home: %w", err)
	}
	if strings.HasPrefix(clean, `\\`) || strings.HasPrefix(clean, `//`) {
		return Layout{}, errors.New("Workflow Home must not use a network path")
	}
	layout := Layout{
		Root: clean, Bin: filepath.Join(clean, "bin"), Versions: filepath.Join(clean, "versions"),
		Config: filepath.Join(clean, "config"), State: filepath.Join(clean, "state"),
		Workspaces: filepath.Join(clean, "workspaces"), Backups: filepath.Join(clean, "backups"),
		Logs: filepath.Join(clean, "logs"),
	}
	layout.CredentialFile = filepath.Join(layout.State, "credentials", "github.pat")
	return layout, nil
}

func (l Layout) Directories() []string {
	return []string{l.Root, l.Bin, l.Versions, l.Config, l.State, l.Workspaces, l.Backups, l.Logs, filepath.Dir(l.CredentialFile)}
}

func (l Layout) Ensure() error {
	if l.Root == "" {
		return errors.New("Workflow Home layout is unresolved")
	}
	for _, directory := range l.Directories() {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create Workflow Home directory %q: %w", directory, err)
		}
	}
	return nil
}
