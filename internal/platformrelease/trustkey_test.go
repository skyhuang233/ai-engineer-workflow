package platformrelease

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareTrustKeyGeneratesOfflinePrivateKeyAndDeterministicPublicArtifact(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	offline := filepath.Join(t.TempDir(), "offline")
	if err := os.MkdirAll(filepath.Join(repository, "trust"), 0o755); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(offline, "platform-release-private-key.pem")
	publicPath := filepath.Join(repository, "trust", "platform-release-public-key.pem")
	first, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privatePath, PublicKeyPath: publicPath, Generate: true})
	if err != nil {
		t.Fatal(err)
	}
	privateRaw, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first.PublicPEM, privateRaw) || strings.Contains(string(first.PublicPEM), "PRIVATE") {
		t.Fatal("public result exposed private key material")
	}
	info, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private key permissions = %v", info.Mode().Perm())
	}
	second, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privatePath, PublicKeyPath: publicPath})
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicKeySHA256 != second.PublicKeySHA256 || !bytes.Equal(first.PublicPEM, second.PublicPEM) {
		t.Fatal("deriving from the same private key was not deterministic")
	}
	block, _ := pem.Decode(second.PublicPEM)
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PublicKey)
	if err != nil || !ok || !isP256(key.Curve) {
		t.Fatalf("public artifact is not P-256: %T, %v", parsed, err)
	}
}

func TestPrepareTrustKeyFailsClosedForUnsafeOrConflictingPaths(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		options TrustKeyOptions
	}{
		{name: "private key inside repository", options: TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: filepath.Join(repository, "private.pem"), PublicKeyPath: filepath.Join(repository, "public.pem"), Generate: true}},
		{name: "public key outside repository", options: TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: filepath.Join(t.TempDir(), "private.pem"), PublicKeyPath: filepath.Join(t.TempDir(), "public.pem"), Generate: true}},
		{name: "generation not explicit", options: TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: filepath.Join(t.TempDir(), "missing.pem"), PublicKeyPath: filepath.Join(repository, "public.pem")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PrepareTrustKey(test.options); err == nil {
				t.Fatal("unsafe key ceremony was accepted")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(repository, "private.pem")); !os.IsNotExist(err) {
		t.Fatalf("rejected private path mutated the repository: %v", err)
	}
}

func TestPrepareTrustKeyRefusesDifferentExistingPublicArtifact(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	offline := filepath.Join(t.TempDir(), "offline")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(offline, "private.pem")
	publicPath := filepath.Join(repository, "public.pem")
	if _, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privatePath, PublicKeyPath: publicPath, Generate: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privatePath, PublicKeyPath: publicPath}); err == nil {
		t.Fatal("different existing public artifact was overwritten")
	}
}

func TestPrepareTrustKeyRefusesLinkedKeyArtifacts(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	offline := filepath.Join(t.TempDir(), "offline")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(offline, "private.pem")
	publicPath := filepath.Join(repository, "public.pem")
	if _, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privatePath, PublicKeyPath: publicPath, Generate: true}); err != nil {
		t.Fatal(err)
	}
	privateLink := filepath.Join(offline, "private-link.pem")
	if err := os.Symlink(privatePath, privateLink); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privateLink, PublicKeyPath: publicPath}); err == nil {
		t.Fatal("linked private key was accepted")
	}
	linkedPublic := filepath.Join(repository, "linked-public.pem")
	if err := os.Symlink(publicPath, linkedPublic); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareTrustKey(TrustKeyOptions{RepositoryRoot: repository, PrivateKeyPath: privatePath, PublicKeyPath: linkedPublic}); err == nil {
		t.Fatal("linked public artifact was accepted")
	}
}
