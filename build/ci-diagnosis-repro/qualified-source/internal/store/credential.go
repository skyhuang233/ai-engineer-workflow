package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type GitHubPATVerification struct {
	FingerprintSHA256 string
	Login             string
	UserID            int64
	Owner             string
	Scopes            []string
	CredentialPath    string
	Status            string
	VerifiedAt        time.Time
}

func (s *Store) RecordGitHubPATVerification(ctx context.Context, value GitHubPATVerification) error {
	if !fingerprintPattern.MatchString(value.FingerprintSHA256) || value.Login == "" || value.UserID <= 0 || value.Owner == "" || len(value.Scopes) == 0 || value.CredentialPath == "" || value.Status == "" || value.VerifiedAt.IsZero() {
		return errors.New("invalid GitHub PAT verification")
	}
	scopes, err := json.Marshal(value.Scopes)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO github_pat_verifications(singleton,fingerprint_sha256,login,user_id,owner,scopes_json,credential_path,status,verified_at) VALUES(1,?,?,?,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET fingerprint_sha256=excluded.fingerprint_sha256,login=excluded.login,user_id=excluded.user_id,owner=excluded.owner,scopes_json=excluded.scopes_json,credential_path=excluded.credential_path,status=excluded.status,verified_at=excluded.verified_at`, value.FingerprintSHA256, value.Login, value.UserID, value.Owner, string(scopes), value.CredentialPath, value.Status, formatTimestamp(value.VerifiedAt))
	return err
}

func (s *Store) GitHubPATVerification(ctx context.Context) (GitHubPATVerification, error) {
	var value GitHubPATVerification
	var scopes, verified string
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint_sha256,login,user_id,owner,scopes_json,credential_path,status,verified_at FROM github_pat_verifications WHERE singleton=1`).Scan(&value.FingerprintSHA256, &value.Login, &value.UserID, &value.Owner, &scopes, &value.CredentialPath, &value.Status, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubPATVerification{}, ErrNotFound
	}
	if err != nil {
		return GitHubPATVerification{}, err
	}
	if err = json.Unmarshal([]byte(scopes), &value.Scopes); err != nil {
		return GitHubPATVerification{}, err
	}
	value.VerifiedAt, err = time.Parse(time.RFC3339Nano, verified)
	return value, err
}
