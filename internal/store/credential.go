package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type GitHubAppVerification struct {
	FingerprintSHA256     string
	AppID                 int64
	InstallationID        int64
	Owner                 string
	IntegrationRepository string
	VerifiedAt            time.Time
}

func (s *Store) RecordGitHubAppVerification(ctx context.Context, verification GitHubAppVerification) error {
	if !fingerprintPattern.MatchString(verification.FingerprintSHA256) ||
		verification.AppID <= 0 || verification.InstallationID <= 0 ||
		verification.Owner == "" || verification.IntegrationRepository == "" || verification.VerifiedAt.IsZero() {
		return errors.New("invalid GitHub App verification")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_credential_verifications(singleton, fingerprint_sha256, app_id, installation_id, owner, integration_repository, verified_at)
VALUES (1, ?, ?, ?, ?, ?, ?)
ON CONFLICT(singleton) DO UPDATE SET fingerprint_sha256=excluded.fingerprint_sha256, owner=excluded.owner,
app_id=excluded.app_id, installation_id=excluded.installation_id,
integration_repository=excluded.integration_repository, verified_at=excluded.verified_at`,
		verification.FingerprintSHA256, verification.AppID, verification.InstallationID, verification.Owner, verification.IntegrationRepository,
		verification.VerifiedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GitHubAppVerification(ctx context.Context) (GitHubAppVerification, error) {
	var verification GitHubAppVerification
	var verifiedAt string
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint_sha256, app_id, installation_id, owner, integration_repository, verified_at
FROM gateway_credential_verifications WHERE singleton = 1`).
		Scan(&verification.FingerprintSHA256, &verification.AppID, &verification.InstallationID, &verification.Owner, &verification.IntegrationRepository, &verifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubAppVerification{}, ErrNotFound
	}
	if err != nil {
		return GitHubAppVerification{}, err
	}
	verification.VerifiedAt, err = time.Parse(time.RFC3339Nano, verifiedAt)
	return verification, err
}
