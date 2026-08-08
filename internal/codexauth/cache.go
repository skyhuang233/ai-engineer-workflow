// Package codexauth manages the credential cache that authenticates Codex
// inside trusted Worker containers.
package codexauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const FileName = "auth.json"

type chatGPTCache struct {
	AuthMode string `json:"auth_mode"`
	APIKey   string `json:"OPENAI_API_KEY"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		AccountID    string `json:"account_id"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type Redactor struct {
	values [][]byte
}

// ValidateChatGPT verifies only the cache shape needed by this workflow. It
// intentionally never returns credential values in an error.
func ValidateChatGPT(source string) error {
	_, _, err := readChatGPT(source)
	return err
}

func readChatGPT(source string) ([]byte, chatGPTCache, error) {
	var cache chatGPTCache
	if !filepath.IsAbs(source) {
		return nil, cache, errors.New("Codex authentication source must be an absolute path")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, cache, fmt.Errorf("inspect Codex authentication source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, cache, errors.New("Codex authentication source must be a regular file")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, cache, fmt.Errorf("read Codex authentication source: %w", err)
	}
	if json.Unmarshal(content, &cache) != nil || cache.AuthMode != "chatgpt" ||
		strings.TrimSpace(cache.Tokens.AccessToken) == "" || strings.TrimSpace(cache.Tokens.AccountID) == "" ||
		strings.TrimSpace(cache.Tokens.IDToken) == "" || strings.TrimSpace(cache.Tokens.RefreshToken) == "" {
		return nil, cache, errors.New("Codex authentication source must contain a valid ChatGPT login cache")
	}
	return content, cache, nil
}

func NewRedactor(source string) (Redactor, error) {
	_, cache, err := readChatGPT(source)
	if err != nil {
		return Redactor{}, err
	}
	values := []string{cache.APIKey, cache.Tokens.AccessToken, cache.Tokens.AccountID, cache.Tokens.IDToken, cache.Tokens.RefreshToken}
	redactor := Redactor{values: make([][]byte, 0, len(values))}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			redactor.values = append(redactor.values, []byte(value))
		}
	}
	return redactor, nil
}

func (r Redactor) Bytes(input []byte) []byte {
	result := append([]byte(nil), input...)
	for _, value := range r.values {
		result = bytes.ReplaceAll(result, value, []byte("[REDACTED]"))
	}
	return result
}

func (r Redactor) String(input string) string {
	return string(r.Bytes([]byte(input)))
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
	content, _, err := readChatGPT(source)
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
