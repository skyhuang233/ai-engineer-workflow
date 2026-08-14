package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type PlatformInstallation struct {
	PlatformVersion             string
	ReleaseManifestDigestSHA256 string
	WorkflowHome                string
	InstalledAt                 time.Time
	VerifiedAt                  time.Time
}

func (s *Store) RecordPlatformInstallation(ctx context.Context, value PlatformInstallation) error {
	if value.PlatformVersion == "" || !fingerprintPattern.MatchString(value.ReleaseManifestDigestSHA256) || value.WorkflowHome == "" || value.InstalledAt.IsZero() || value.VerifiedAt.IsZero() {
		return errors.New("invalid Platform Installation")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO platform_installation(singleton,platform_version,release_manifest_digest,workflow_home,installed_at,verified_at) VALUES(1,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET platform_version=excluded.platform_version,release_manifest_digest=excluded.release_manifest_digest,workflow_home=excluded.workflow_home,installed_at=excluded.installed_at,verified_at=excluded.verified_at`, value.PlatformVersion, value.ReleaseManifestDigestSHA256, value.WorkflowHome, formatTimestamp(value.InstalledAt), formatTimestamp(value.VerifiedAt))
	return err
}
func (s *Store) PlatformInstallation(ctx context.Context) (PlatformInstallation, error) {
	var value PlatformInstallation
	var installed, verified string
	err := s.db.QueryRowContext(ctx, `SELECT platform_version,release_manifest_digest,workflow_home,installed_at,verified_at FROM platform_installation WHERE singleton=1`).Scan(&value.PlatformVersion, &value.ReleaseManifestDigestSHA256, &value.WorkflowHome, &installed, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return PlatformInstallation{}, ErrNotFound
	}
	if err != nil {
		return PlatformInstallation{}, err
	}
	value.InstalledAt, err = time.Parse(time.RFC3339Nano, installed)
	if err != nil {
		return PlatformInstallation{}, err
	}
	value.VerifiedAt, err = time.Parse(time.RFC3339Nano, verified)
	return value, err
}
