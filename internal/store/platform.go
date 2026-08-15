package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type PlatformInstallation struct {
	PlatformVersion                   string
	ReleaseManifestDigestSHA256       string
	PlatformSetupContractDigestSHA256 string
	WorkflowCLISHA256                 string
	ControlPlanePlanDigestSHA256      string
	WorkflowHome                      string
	InstalledAt                       time.Time
	VerifiedAt                        time.Time
}

func (s *Store) RecordPlatformInstallation(ctx context.Context, value PlatformInstallation) error {
	if value.PlatformVersion == "" || !fingerprintPattern.MatchString(value.ReleaseManifestDigestSHA256) || !fingerprintPattern.MatchString(value.PlatformSetupContractDigestSHA256) || !fingerprintPattern.MatchString(value.WorkflowCLISHA256) || value.WorkflowHome == "" || value.InstalledAt.IsZero() || value.VerifiedAt.IsZero() {
		return errors.New("invalid Platform Installation")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO platform_installation(singleton,platform_version,release_manifest_digest,platform_setup_contract_digest,workflow_cli_sha256,control_plane_plan_digest,workflow_home,installed_at,verified_at) VALUES(1,?,?,?,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET platform_version=excluded.platform_version,release_manifest_digest=excluded.release_manifest_digest,platform_setup_contract_digest=excluded.platform_setup_contract_digest,workflow_cli_sha256=excluded.workflow_cli_sha256,control_plane_plan_digest='',workflow_home=excluded.workflow_home,installed_at=excluded.installed_at,verified_at=excluded.verified_at`, value.PlatformVersion, value.ReleaseManifestDigestSHA256, value.PlatformSetupContractDigestSHA256, value.WorkflowCLISHA256, value.ControlPlanePlanDigestSHA256, value.WorkflowHome, formatTimestamp(value.InstalledAt), formatTimestamp(value.VerifiedAt))
	return err
}
func (s *Store) PlatformInstallation(ctx context.Context) (PlatformInstallation, error) {
	var value PlatformInstallation
	var installed, verified string
	err := s.db.QueryRowContext(ctx, `SELECT platform_version,release_manifest_digest,platform_setup_contract_digest,workflow_cli_sha256,control_plane_plan_digest,workflow_home,installed_at,verified_at FROM platform_installation WHERE singleton=1`).Scan(&value.PlatformVersion, &value.ReleaseManifestDigestSHA256, &value.PlatformSetupContractDigestSHA256, &value.WorkflowCLISHA256, &value.ControlPlanePlanDigestSHA256, &value.WorkflowHome, &installed, &verified)
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

func (s *Store) AuthorizeControlPlane(ctx context.Context, expected PlatformInstallation, planDigest string) error {
	if !fingerprintPattern.MatchString(planDigest) || expected.PlatformVersion == "" || !fingerprintPattern.MatchString(expected.ReleaseManifestDigestSHA256) || !fingerprintPattern.MatchString(expected.PlatformSetupContractDigestSHA256) || !fingerprintPattern.MatchString(expected.WorkflowCLISHA256) {
		return errors.New("invalid Control Plane authorization")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE platform_installation SET control_plane_plan_digest=?,verified_at=? WHERE singleton=1 AND platform_version=? AND release_manifest_digest=? AND platform_setup_contract_digest=? AND workflow_cli_sha256=?`, planDigest, formatTimestamp(time.Now().UTC()), expected.PlatformVersion, expected.ReleaseManifestDigestSHA256, expected.PlatformSetupContractDigestSHA256, expected.WorkflowCLISHA256)
	if err != nil {
		return err
	}
	updated, _ := result.RowsAffected()
	if updated != 1 {
		return errors.New("Platform Installation pins drifted before Control Plane authorization")
	}
	return nil
}
