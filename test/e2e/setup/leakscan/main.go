// Command leakscan is a repository-owned production-qualification helper. It
// receives the PAT only on stdin and rejects credential material outside the
// one durable identity field approved by the credential contract.
package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type scanner struct {
	token, fingerprint string
	mainDatabase       string
	credentialFile     string
	allowedFingerprint int
	rawMainFingerprint int
}

func main() {
	workflowHome := flag.String("workflow-home", "", "absolute Workflow Home")
	evidenceRoot := flag.String("evidence-root", "", "absolute setup evidence root")
	flag.Parse()
	tokenBytes, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		fatal(errors.New("PAT is required on stdin"))
	}
	home, err := filepath.Abs(*workflowHome)
	if err != nil || !filepath.IsAbs(home) {
		fatal(errors.New("workflow-home must be absolute"))
	}
	evidence, err := filepath.Abs(*evidenceRoot)
	if err != nil || !filepath.IsAbs(evidence) {
		fatal(errors.New("evidence-root must be absolute"))
	}
	sum := sha256.Sum256([]byte(token))
	s := scanner{
		token:          token,
		fingerprint:    hex.EncodeToString(sum[:]),
		mainDatabase:   filepath.Clean(filepath.Join(home, "state", "workflow.db")),
		credentialFile: filepath.Clean(filepath.Join(home, "state", "credentials", "github.pat")),
	}
	if err := s.compactMainDatabase(); err != nil {
		fatal(err)
	}
	for _, root := range []string{home, evidence} {
		if err := s.scanRoot(root); err != nil {
			fatal(err)
		}
	}
	if s.allowedFingerprint != 1 {
		fatal(fmt.Errorf("github_pat_verifications.fingerprint_sha256 durable singleton count = %d, want 1", s.allowedFingerprint))
	}
	if s.rawMainFingerprint != 1 {
		fatal(fmt.Errorf("compressed Workflow database raw fingerprint count = %d, want 1", s.rawMainFingerprint))
	}
}

func (s *scanner) compactMainDatabase() error {
	if _, err := os.Stat(s.mainDatabase); err != nil {
		return fmt.Errorf("Workflow database is unavailable for credential scan: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.mainDatabase))
	if err != nil {
		return err
	}
	defer db.Close()
	var busy, logFrames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint Workflow database before credential scan: %w", err)
	}
	if busy != 0 {
		return errors.New("Workflow database is busy during credential scan checkpoint")
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("compact Workflow database before raw credential scan: %w", err)
	}
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint compacted Workflow database before credential scan: %w", err)
	}
	if busy != 0 {
		return errors.New("Workflow database is busy after credential scan compaction")
	}
	return nil
}

func (s *scanner) scanRoot(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		clean := filepath.Clean(path)
		if clean == s.credentialFile {
			body, err := os.ReadFile(clean)
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(body)) != s.token {
				return fmt.Errorf("Workflow Home credential file differs from the exact PAT")
			}
			return nil
		}
		body, err := os.ReadFile(clean)
		if err != nil {
			return err
		}
		if clean == s.mainDatabase {
			if err := s.scanSQLite(clean, true); err != nil {
				return err
			}
			if err := s.rejectNeedles(clean, body, true); err != nil {
				return err
			}
			s.rawMainFingerprint = bytes.Count(body, []byte(s.fingerprint))
			return nil
		}
		if bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
			if err := s.scanSQLite(clean, false); err != nil {
				return err
			}
		}
		return s.rejectNeedles(clean, body, false)
	})
}

func (s *scanner) scanSQLite(path string, main bool) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	tables, err := db.Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("open qualification SQLite %s: %w", path, err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			return err
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, table := range names {
		columns, err := sqliteColumns(db, table)
		if err != nil {
			return err
		}
		for _, column := range columns {
			query := `SELECT CAST(` + quoteIdentifier(column) + ` AS TEXT) FROM ` + quoteIdentifier(table) + ` WHERE ` + quoteIdentifier(column) + ` IS NOT NULL`
			rows, err := db.Query(query)
			if err != nil {
				return fmt.Errorf("scan %s.%s: %w", table, column, err)
			}
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					_ = rows.Close()
					return err
				}
				allowed := main && table == "github_pat_verifications" && column == "fingerprint_sha256" && value == s.fingerprint
				if allowed {
					s.allowedFingerprint++
					continue
				}
				if err := s.rejectNeedles(path+":"+table+"."+column, []byte(value), false); err != nil {
					_ = rows.Close()
					return err
				}
			}
			_ = rows.Close()
		}
	}
	return nil
}

func sqliteColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`PRAGMA table_info(` + quoteIdentifier(table) + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func (s *scanner) rejectNeedles(location string, body []byte, allowMainFingerprint bool) error {
	basicCredential := []byte("x-access-token:" + s.token)
	for label, needle := range map[string]string{
		"exact PAT":             s.token,
		"exact PAT fingerprint": s.fingerprint,
		"Git Basic credential":  base64.StdEncoding.EncodeToString(basicCredential),
	} {
		if allowMainFingerprint && label == "exact PAT fingerprint" {
			continue
		}
		if bytes.Contains(body, []byte(needle)) {
			return fmt.Errorf("credential leak in %s: %s", location, label)
		}
	}
	lower := bytes.ToLower(body)
	if bytes.Contains(lower, []byte("authorization: bearer")) || bytes.Contains(lower, []byte("authorization: basic")) {
		return fmt.Errorf("credential leak in %s: Authorization header", location)
	}
	return nil
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
