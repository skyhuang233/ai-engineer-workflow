package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustKeyCommandEmitsOnlyPublicMetadata(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(repository, "trust"), 0o755); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(t.TempDir(), "offline", "private.pem")
	publicPath := filepath.Join(repository, "trust", "public.pem")
	var output bytes.Buffer
	if err := runTrustKey([]string{"--repository-root", repository, "--private-key", privatePath, "--public-key", publicPath, "--generate"}, &output); err != nil {
		t.Fatal(err)
	}
	privateRaw, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), privateRaw) || strings.Contains(output.String(), "PRIVATE KEY") || strings.Contains(output.String(), privatePath) {
		t.Fatalf("trust-key output exposed private material or location: %s", output.String())
	}
	var result struct {
		PublicKeyPath   string `json:"public_key_path"`
		PublicKeySHA256 string `json:"public_key_sha256"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.PublicKeyPath != publicPath || len(result.PublicKeySHA256) != 64 {
		t.Fatalf("trust-key public result = %#v", result)
	}
}

func TestTrustKeyCommandFailsWithoutExplicitModeInputs(t *testing.T) {
	if err := runTrustKey(nil, &bytes.Buffer{}); err == nil {
		t.Fatal("trust-key accepted implicit paths or generation")
	}
}
