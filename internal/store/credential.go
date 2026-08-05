package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type GatewayCredentialVerification struct {
	FingerprintSHA256     string
	Owner                 string
	IntegrationRepository string
	VerifiedAt            time.Time
}

func (s *Store) RecordGatewayCredentialVerification(ctx context.Context, verification GatewayCredentialVerification) error {
	if !fingerprintPattern.MatchString(verification.FingerprintSHA256) ||
		verification.Owner == "" || verification.IntegrationRepository == "" || verification.VerifiedAt.IsZero() {
		return errors.New("invalid Gateway Credential verification")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_credential_verifications(singleton, fingerprint_sha256, owner, integration_repository, verified_at)
VALUES (1, ?, ?, ?, ?)
ON CONFLICT(singleton) DO UPDATE SET fingerprint_sha256=excluded.fingerprint_sha256, owner=excluded.owner,
integration_repository=excluded.integration_repository, verified_at=excluded.verified_at`,
		verification.FingerprintSHA256, verification.Owner, verification.IntegrationRepository,
		verification.VerifiedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GatewayCredentialVerification(ctx context.Context) (GatewayCredentialVerification, error) {
	var verification GatewayCredentialVerification
	var verifiedAt string
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint_sha256, owner, integration_repository, verified_at
FROM gateway_credential_verifications WHERE singleton = 1`).
		Scan(&verification.FingerprintSHA256, &verification.Owner, &verification.IntegrationRepository, &verifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GatewayCredentialVerification{}, ErrNotFound
	}
	if err != nil {
		return GatewayCredentialVerification{}, err
	}
	verification.VerifiedAt, err = time.Parse(time.RFC3339Nano, verifiedAt)
	return verification, err
}
