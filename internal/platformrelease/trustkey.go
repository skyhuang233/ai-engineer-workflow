package platformrelease

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type TrustKeyOptions struct {
	RepositoryRoot string
	PrivateKeyPath string
	PublicKeyPath  string
	Generate       bool
}

type TrustKeyResult struct {
	PublicKeyPath   string `json:"public_key_path"`
	PublicKeySHA256 string `json:"public_key_sha256"`
	PublicPEM       []byte `json:"-"`
}

// PrepareTrustKey creates an offline P-256 private key only when Generate is
// explicit, or deterministically derives the public artifact from an existing
// private key. Private keys are required to live outside the repository.
func PrepareTrustKey(options TrustKeyOptions) (TrustKeyResult, error) {
	repository, err := existingDirectory(options.RepositoryRoot)
	if err != nil {
		return TrustKeyResult{}, fmt.Errorf("resolve repository root: %w", err)
	}
	privatePath, err := filepath.Abs(strings.TrimSpace(options.PrivateKeyPath))
	if err != nil || strings.TrimSpace(options.PrivateKeyPath) == "" {
		return TrustKeyResult{}, errors.New("offline private key path is required")
	}
	publicPath, err := filepath.Abs(strings.TrimSpace(options.PublicKeyPath))
	if err != nil || strings.TrimSpace(options.PublicKeyPath) == "" {
		return TrustKeyResult{}, errors.New("repository public key path is required")
	}
	// Reject unsafe lexical targets before generation can create a directory.
	// Canonical checks below repeat the boundary test after resolving parents.
	if pathWithin(repository, privatePath) {
		return TrustKeyResult{}, errors.New("private key path must be outside the repository")
	}
	if !pathWithin(repository, publicPath) {
		return TrustKeyResult{}, errors.New("public trust artifact must be inside the repository")
	}
	privatePath, err = canonicalTarget(privatePath, options.Generate, 0o700)
	if err != nil {
		return TrustKeyResult{}, fmt.Errorf("resolve offline private key path: %w", err)
	}
	publicPath, err = canonicalTarget(publicPath, false, 0)
	if err != nil {
		return TrustKeyResult{}, fmt.Errorf("resolve public trust artifact path: %w", err)
	}
	if pathWithin(repository, privatePath) {
		return TrustKeyResult{}, errors.New("private key path must be outside the repository")
	}
	if !pathWithin(repository, publicPath) {
		return TrustKeyResult{}, errors.New("public trust artifact must be inside the repository")
	}
	if strings.EqualFold(privatePath, publicPath) {
		return TrustKeyResult{}, errors.New("private and public key paths must differ")
	}
	if err := requireRegularOrMissing(privatePath); err != nil {
		return TrustKeyResult{}, fmt.Errorf("inspect offline private key: %w", err)
	}
	if err := requireRegularOrMissing(publicPath); err != nil {
		return TrustKeyResult{}, fmt.Errorf("inspect public trust artifact: %w", err)
	}

	var key *ecdsa.PrivateKey
	if options.Generate {
		if _, err := os.Stat(privatePath); err == nil {
			return TrustKeyResult{}, errors.New("refusing to overwrite existing private key")
		} else if !errors.Is(err, os.ErrNotExist) {
			return TrustKeyResult{}, fmt.Errorf("inspect private key path: %w", err)
		}
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return TrustKeyResult{}, fmt.Errorf("generate P-256 key: %w", err)
		}
		if err := writePrivateKeyExclusive(privatePath, key); err != nil {
			return TrustKeyResult{}, err
		}
	} else {
		if err := securePrivateKey(privatePath); err != nil {
			return TrustKeyResult{}, fmt.Errorf("restrict existing offline private key: %w", err)
		}
		key, err = readP256PrivateKey(privatePath)
		if err != nil {
			return TrustKeyResult{}, err
		}
	}

	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return TrustKeyResult{}, fmt.Errorf("marshal P-256 public key: %w", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := writePublicArtifact(publicPath, publicPEM); err != nil {
		return TrustKeyResult{}, err
	}
	digest := sha256.Sum256(publicDER)
	return TrustKeyResult{PublicKeyPath: publicPath, PublicKeySHA256: hex.EncodeToString(digest[:]), PublicPEM: publicPEM}, nil
}

func requireRegularOrMissing(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path must be a regular file and not a symbolic link")
	}
	return nil
}

func existingDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil || strings.TrimSpace(path) == "" {
		return "", errors.New("repository root is required")
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("repository root is not a directory")
	}
	return filepath.EvalSymlinks(absolute)
}

func canonicalTarget(path string, createParent bool, mode os.FileMode) (string, error) {
	parent := filepath.Dir(path)
	if createParent {
		if err := os.MkdirAll(parent, mode); err != nil {
			return "", err
		}
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.New("parent directory must exist and contain no unresolved links")
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func writePrivateKeyExclusive(path string, key *ecdsa.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create offline key directory: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal P-256 private key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create offline private key without overwrite: %w", err)
	}
	if _, err := file.Write(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		_ = file.Close()
		return fmt.Errorf("write offline private key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync offline private key: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close offline private key: %w", err)
	}
	if err := securePrivateKey(path); err != nil {
		removeErr := os.Remove(path)
		return errors.Join(err, removeErr)
	}
	return nil
}

func readP256PrivateKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read offline private key: %w", err)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "EC PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("offline private key must be one EC PRIVATE KEY PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil || !isP256(key.Curve) {
		return nil, errors.New("offline private key must use ECDSA P-256")
	}
	return key, nil
}

func writePublicArtifact(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, content) {
			return nil
		}
		return errors.New("refusing to overwrite a different public trust artifact")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect public trust artifact: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create public trust directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create public trust artifact without overwrite: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return errors.Join(fmt.Errorf("write public trust artifact: %w", err), os.Remove(path))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.Join(fmt.Errorf("sync public trust artifact: %w", err), os.Remove(path))
	}
	return file.Close()
}
