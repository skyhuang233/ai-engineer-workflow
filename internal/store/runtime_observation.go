package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type ControlPlaneRuntimeObservation struct {
	PID              int
	ProcessStartedAt time.Time
	EndpointsJSON    string
	PlatformVersion  string
	PlanDigestSHA256 string
	ObservedAt       time.Time
}

func (s *Store) RecordControlPlaneRuntimeObservation(ctx context.Context, value ControlPlaneRuntimeObservation) error {
	if value.PID <= 0 || value.ProcessStartedAt.IsZero() || value.EndpointsJSON == "" || value.PlatformVersion == "" || !fingerprintPattern.MatchString(value.PlanDigestSHA256) || value.ObservedAt.IsZero() {
		return errors.New("invalid Control Plane Runtime Record")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO control_plane_runtime_observation(singleton,pid,process_started_at,endpoints_json,platform_version,plan_digest_sha256,observed_at) VALUES(1,?,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET pid=excluded.pid,process_started_at=excluded.process_started_at,endpoints_json=excluded.endpoints_json,platform_version=excluded.platform_version,plan_digest_sha256=excluded.plan_digest_sha256,observed_at=excluded.observed_at`, value.PID, formatTimestamp(value.ProcessStartedAt), value.EndpointsJSON, value.PlatformVersion, value.PlanDigestSHA256, formatTimestamp(value.ObservedAt))
	return err
}
func (s *Store) ControlPlaneRuntimeObservation(ctx context.Context) (ControlPlaneRuntimeObservation, error) {
	var value ControlPlaneRuntimeObservation
	var started, observed string
	err := s.db.QueryRowContext(ctx, `SELECT pid,process_started_at,endpoints_json,platform_version,plan_digest_sha256,observed_at FROM control_plane_runtime_observation WHERE singleton=1`).Scan(&value.PID, &started, &value.EndpointsJSON, &value.PlatformVersion, &value.PlanDigestSHA256, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlPlaneRuntimeObservation{}, ErrNotFound
	}
	if err != nil {
		return ControlPlaneRuntimeObservation{}, err
	}
	value.ProcessStartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return ControlPlaneRuntimeObservation{}, err
	}
	value.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
	return value, err
}
