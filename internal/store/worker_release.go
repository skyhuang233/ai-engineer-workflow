package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	fullCommitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageDigestPattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9_.-]+/[a-z0-9_./-]+@sha256:[0-9a-f]{64}$`)
)

type WorkerRelease struct {
	Version      string
	SourceCommit string
	ImageDigest  string
	ManifestJSON string
	VerifiedAt   time.Time
	ActivatedAt  time.Time
}

func (s *Store) ActivateWorkerRelease(ctx context.Context, release WorkerRelease) error {
	if release.Version == "" || release.ManifestJSON == "" ||
		!fullCommitPattern.MatchString(release.SourceCommit) ||
		!imageDigestPattern.MatchString(release.ImageDigest) {
		return errors.New("invalid Worker Release")
	}
	if release.VerifiedAt.IsZero() {
		return errors.New("Worker Release verification time is required")
	}
	release.VerifiedAt = release.VerifiedAt.UTC()
	if release.ActivatedAt.IsZero() {
		release.ActivatedAt = release.VerifiedAt
	} else {
		release.ActivatedAt = release.ActivatedAt.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_releases(image_digest, version, source_commit, manifest_json, verified_at, activated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(image_digest) DO UPDATE SET version=excluded.version, source_commit=excluded.source_commit,
manifest_json=excluded.manifest_json, verified_at=excluded.verified_at, activated_at=excluded.activated_at`,
		release.ImageDigest, release.Version, release.SourceCommit, release.ManifestJSON,
		release.VerifiedAt.Format(time.RFC3339Nano), release.ActivatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("record Worker Release: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO active_worker_image(singleton, image_digest) VALUES (1, ?)
ON CONFLICT(singleton) DO UPDATE SET image_digest=excluded.image_digest`, release.ImageDigest); err != nil {
		return fmt.Errorf("activate Worker Release: %w", err)
	}
	return tx.Commit()
}

func (s *Store) ActiveWorkerRelease(ctx context.Context) (WorkerRelease, error) {
	var release WorkerRelease
	var verified, activated string
	err := s.db.QueryRowContext(ctx, `SELECT r.version, r.source_commit, r.image_digest, r.manifest_json, r.verified_at, r.activated_at
FROM active_worker_image a JOIN worker_releases r ON r.image_digest = a.image_digest WHERE a.singleton = 1`).
		Scan(&release.Version, &release.SourceCommit, &release.ImageDigest, &release.ManifestJSON, &verified, &activated)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerRelease{}, ErrNotFound
	}
	if err != nil {
		return WorkerRelease{}, err
	}
	release.VerifiedAt, err = time.Parse(time.RFC3339Nano, verified)
	if err != nil {
		return WorkerRelease{}, err
	}
	release.ActivatedAt, err = time.Parse(time.RFC3339Nano, activated)
	return release, err
}
