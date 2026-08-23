package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileStore intentionally stores the Control Plane PAT as plaintext in its
// dedicated Workflow Home file. Callers must never include the returned value
// in plans, results, logs, or process arguments.
type FileStore struct{ path string }

func NewFileStore(path string) *FileStore { return &FileStore{path: filepath.Clean(path)} }
func (s *FileStore) Path() string         { return s.path }

func (s *FileStore) Get(_ context.Context, target string) (string, error) {
	if target != GatewayTarget || s.path == "" {
		return "", ErrNotFound
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read Control Plane GitHub credential: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", ErrNotFound
	}
	return secret, nil
}

func (s *FileStore) Set(_ context.Context, target, secret string) error {
	secret = strings.TrimSpace(secret)
	if target != GatewayTarget || s.path == "" || secret == "" {
		return errors.New("credential target, path, and secret are required")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".github.pat-*.tmp")
	if err != nil {
		return fmt.Errorf("create credential file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("restrict credential file: %w", err)
	}
	if _, err := temporary.WriteString(secret + "\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync credential file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close credential file: %w", err)
	}
	if err := replaceCredentialFile(temporaryPath, s.path); err != nil {
		return fmt.Errorf("publish credential file: %w", err)
	}
	return nil
}
