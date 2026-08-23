package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type RepositoryAdmission struct {
	Repository                 string
	OnboardingPlanDigestSHA256 string
	ContractVersion            string
	ManifestDigestSHA256       string
	Eligible                   bool
	SuspensionReason           string
	VerifiedAt                 time.Time
}

type repositoryRecordExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func validateRepositoryAdmission(value RepositoryAdmission) error {
	if value.Repository == "" || !fingerprintPattern.MatchString(value.OnboardingPlanDigestSHA256) || value.ContractVersion == "" || !fingerprintPattern.MatchString(value.ManifestDigestSHA256) || value.VerifiedAt.IsZero() {
		return errors.New("invalid Repository Admission")
	}
	return nil
}

func recordRepositoryAdmission(ctx context.Context, executor repositoryRecordExecutor, value RepositoryAdmission) error {
	if err := validateRepositoryAdmission(value); err != nil {
		return err
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO repository_admissions(repository,onboarding_plan_digest_sha256,contract_version,manifest_digest_sha256,eligible,suspension_reason,verified_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(repository) DO UPDATE SET onboarding_plan_digest_sha256=excluded.onboarding_plan_digest_sha256,contract_version=excluded.contract_version,manifest_digest_sha256=excluded.manifest_digest_sha256,eligible=excluded.eligible,suspension_reason=excluded.suspension_reason,verified_at=excluded.verified_at`, value.Repository, value.OnboardingPlanDigestSHA256, value.ContractVersion, value.ManifestDigestSHA256, boolInt(value.Eligible), value.SuspensionReason, formatTimestamp(value.VerifiedAt))
	return err
}

func (s *Store) RecordRepositoryAdmission(ctx context.Context, value RepositoryAdmission) error {
	return recordRepositoryAdmission(ctx, s.db, value)
}

func (s *Store) RecordRepositoryAdmissionWithInitialRuntimeConfiguration(ctx context.Context, admission RepositoryAdmission, runtime RepositoryRuntimeConfiguration) error {
	if err := validateRepositoryAdmission(admission); err != nil {
		return err
	}
	if err := runtime.Validate(); err != nil {
		return err
	}
	if admission.Repository != runtime.Repository {
		return errors.New("Repository Admission and runtime configuration repositories differ")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordRepositoryAdmission(ctx, tx, admission); err != nil {
		return err
	}
	if err := recordRepositoryRuntimeConfiguration(ctx, tx, runtime, repositoryRuntimePreserveConflict); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RepositoryAdmission(ctx context.Context, repository string) (RepositoryAdmission, error) {
	var value RepositoryAdmission
	var eligible int
	var verified string
	err := s.db.QueryRowContext(ctx, `SELECT repository,onboarding_plan_digest_sha256,contract_version,manifest_digest_sha256,eligible,suspension_reason,verified_at FROM repository_admissions WHERE repository=?`, repository).Scan(&value.Repository, &value.OnboardingPlanDigestSHA256, &value.ContractVersion, &value.ManifestDigestSHA256, &eligible, &value.SuspensionReason, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryAdmission{}, ErrNotFound
	}
	if err != nil {
		return RepositoryAdmission{}, err
	}
	value.Eligible = eligible == 1
	value.VerifiedAt, err = time.Parse(time.RFC3339Nano, verified)
	return value, err
}

func (s *Store) RepositoryAdmissions(ctx context.Context) ([]RepositoryAdmission, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository,onboarding_plan_digest_sha256,contract_version,manifest_digest_sha256,eligible,suspension_reason,verified_at FROM repository_admissions ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RepositoryAdmission
	for rows.Next() {
		var value RepositoryAdmission
		var eligible int
		var verified string
		if err := rows.Scan(&value.Repository, &value.OnboardingPlanDigestSHA256, &value.ContractVersion, &value.ManifestDigestSHA256, &eligible, &value.SuspensionReason, &verified); err != nil {
			return nil, err
		}
		value.Eligible = eligible == 1
		value.VerifiedAt, err = time.Parse(time.RFC3339Nano, verified)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
