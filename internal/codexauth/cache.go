// Package codexauth manages the credential cache that authenticates Codex
// inside trusted Worker containers.
package codexauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const FileName = "auth.json"

// ValidateChatGPT verifies only the cache shape needed by this workflow. It
// intentionally never returns credential values in an error.
func ValidateChatGPT(source string) error {
	_, err := readChatGPT(source)
	return err
}

func readChatGPT(source string) ([]byte, error) {
	if !filepath.IsAbs(source) {
		return nil, errors.New("Codex authentication source must be an absolute path")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex authentication source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Codex authentication source must be a regular file")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("read Codex authentication source: %w", err)
	}
	var cache struct {
		AuthMode string                     `json:"auth_mode"`
		Tokens   map[string]json.RawMessage `json:"tokens"`
	}
	if json.Unmarshal(content, &cache) != nil || cache.AuthMode != "chatgpt" || len(cache.Tokens) == 0 {
		return nil, errors.New("Codex authentication source must contain a valid ChatGPT login cache")
	}
	return content, nil
}

// Seed copies the trusted host credential cache into a new CODEX_HOME. An
// existing session cache is authoritative because Codex may have refreshed it.
func Seed(source, codexHome string) error {
	if !filepath.IsAbs(codexHome) {
		return errors.New("Codex home must be an absolute path")
	}
	destination := filepath.Join(codexHome, FileName)
	if info, err := os.Stat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("Ticket Session Codex authentication cache must be a regular file")
		}
		return ValidateChatGPT(destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Ticket Session Codex authentication cache: %w", err)
	}
	content, err := readChatGPT(source)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(codexHome, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create Ticket Session Codex authentication cache: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect Ticket Session Codex authentication cache: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("copy Codex authentication cache: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Ticket Session Codex authentication cache: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Ticket Session Codex authentication cache: %w", err)
	}
	if err := os.Link(temporaryPath, destination); errors.Is(err, os.ErrExist) {
		return ValidateChatGPT(destination)
	} else if err != nil {
		return fmt.Errorf("publish Ticket Session Codex authentication cache: %w", err)
	}
	return nil
}
