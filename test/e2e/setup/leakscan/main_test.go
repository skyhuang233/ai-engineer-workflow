package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScannerAllowsOnlyCredentialFileAndDurableVerificationFingerprint(t *testing.T) {
	s, home, evidence := newScannerFixture(t)
	if err := s.compactMainDatabase(); err != nil {
		t.Fatal(err)
	}
	if err := s.scanRoot(home); err != nil {
		t.Fatal(err)
	}
	if err := s.scanRoot(evidence); err != nil {
		t.Fatal(err)
	}
	if s.allowedFingerprint != 1 || s.rawMainFingerprint != 1 {
		t.Fatalf("allowed durable fingerprint semantic=%d raw=%d", s.allowedFingerprint, s.rawMainFingerprint)
	}
}

func TestScannerVacuumRemovesDeletedFingerprintPagesBeforeRawScan(t *testing.T) {
	s, home, evidence := newScannerFixture(t)
	database, err := sql.Open("sqlite", s.mainDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE deleted_evidence(value TEXT); INSERT INTO deleted_evidence(value) VALUES(?); DELETE FROM deleted_evidence`, s.fingerprint); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.compactMainDatabase(); err != nil {
		t.Fatal(err)
	}
	if err := s.scanRoot(home); err != nil {
		t.Fatal(err)
	}
	if err := s.scanRoot(evidence); err != nil {
		t.Fatal(err)
	}
	if s.rawMainFingerprint != 1 {
		t.Fatalf("compressed database retained deleted fingerprint pages: %d", s.rawMainFingerprint)
	}
}

func TestScannerDoesNotExemptWALOrSHMBytes(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			s, home, _ := newScannerFixture(t)
			if err := s.compactMainDatabase(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.mainDatabase+suffix, []byte("Authorization: Bearer "+s.token), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.scanRoot(home); err == nil || !strings.Contains(err.Error(), suffix) {
				t.Fatalf("credential-bearing SQLite sidecar accepted: %v", err)
			}
		})
	}
}

func TestScannerRejectsCredentialMaterialAcrossQualificationBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, relative, content string
	}{
		{name: "workflow log token", relative: `home/logs/control-plane.log`, content: "token"},
		{name: "plan fingerprint", relative: `home/plans/approved.json`, content: "fingerprint"},
		{name: "result authorization", relative: `home/results/result.json`, content: "Authorization: Bearer token"},
		{name: "backup token", relative: `home/backups/metadata.json`, content: "token"},
		{name: "process environment fingerprint", relative: `evidence/process-environment.txt`, content: "fingerprint"},
		{name: "docker inspect token", relative: `evidence/docker-inspect.json`, content: "token"},
		{name: "worker container fingerprint", relative: `evidence/docker-containers.json`, content: "fingerprint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, home, evidence := newScannerFixture(t)
			content := strings.NewReplacer("token", s.token, "fingerprint", s.fingerprint).Replace(test.content)
			parts := strings.SplitN(test.relative, "/", 2)
			root := home
			if parts[0] == "evidence" {
				root = evidence
			}
			path := filepath.Join(root, filepath.FromSlash(parts[1]))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.scanRoot(home); err == nil {
				if err = s.scanRoot(evidence); err == nil {
					t.Fatal("credential material was accepted")
				}
			}
		})
	}
}

func TestScannerRejectsFingerprintInAnyOtherSQLiteField(t *testing.T) {
	s, home, _ := newScannerFixture(t)
	database, err := sql.Open("sqlite", s.mainDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE setup_plans(body TEXT); INSERT INTO setup_plans(body) VALUES(?)`, s.fingerprint); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.scanRoot(home); err == nil || !strings.Contains(err.Error(), "setup_plans.body") {
		t.Fatalf("other SQLite fingerprint = %v", err)
	}
}

func newScannerFixture(t *testing.T) (*scanner, string, string) {
	t.Helper()
	root := t.TempDir()
	home, evidence := filepath.Join(root, "workflow-home"), filepath.Join(root, "evidence")
	credential := filepath.Join(home, "state", "credentials", "github.pat")
	databasePath := filepath.Join(home, "state", "workflow.db")
	if err := os.MkdirAll(filepath.Dir(credential), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	const token = "ghp_round11_exact_secret"
	if err := os.WriteFile(credential, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	fingerprint := hex.EncodeToString(sum[:])
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE github_pat_verifications(fingerprint_sha256 TEXT NOT NULL); INSERT INTO github_pat_verifications(fingerprint_sha256) VALUES(?)`, fingerprint); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return &scanner{token: token, fingerprint: fingerprint, mainDatabase: databasePath, credentialFile: credential}, home, evidence
}
