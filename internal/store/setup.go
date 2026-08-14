package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrSetupPlanConflict = errors.New("immutable Setup Plan conflicts with existing record")

type SetupPlanRecord struct {
	PlanID        string
	Kind          string
	SchemaVersion int
	Target        string
	DigestSHA256  string
	CanonicalJSON string
	Projection    string
	CreatedAt     time.Time
}

type SetupExecutionResult struct {
	ResultID    int64
	PlanID      string
	Attempt     int
	Status      string
	EffectsJSON string
	Diagnostic  string
	StartedAt   time.Time
	CompletedAt time.Time
}

func (s *Store) RecordSetupPlan(ctx context.Context, plan SetupPlanRecord) error {
	if plan.PlanID == "" || plan.SchemaVersion <= 0 || plan.Target == "" || !fingerprintPattern.MatchString(plan.DigestSHA256) || plan.CanonicalJSON == "" || plan.Projection == "" || plan.CreatedAt.IsZero() {
		return errors.New("invalid Setup Plan")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO setup_plans(plan_id,kind,schema_version,target,digest_sha256,canonical_json,projection,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(plan_id) DO NOTHING`, plan.PlanID, plan.Kind, plan.SchemaVersion, plan.Target, plan.DigestSHA256, plan.CanonicalJSON, plan.Projection, formatTimestamp(plan.CreatedAt))
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 1 {
		return nil
	}
	var existing SetupPlanRecord
	var created string
	err = s.db.QueryRowContext(ctx, `SELECT plan_id,kind,schema_version,target,digest_sha256,canonical_json,projection,created_at FROM setup_plans WHERE plan_id=?`, plan.PlanID).Scan(&existing.PlanID, &existing.Kind, &existing.SchemaVersion, &existing.Target, &existing.DigestSHA256, &existing.CanonicalJSON, &existing.Projection, &created)
	if err != nil {
		return err
	}
	if existing.Kind != plan.Kind || existing.SchemaVersion != plan.SchemaVersion || existing.Target != plan.Target || existing.DigestSHA256 != plan.DigestSHA256 || existing.CanonicalJSON != plan.CanonicalJSON || existing.Projection != plan.Projection {
		return ErrSetupPlanConflict
	}
	return nil
}

func (s *Store) AppendSetupExecutionResult(ctx context.Context, result SetupExecutionResult) error {
	if result.PlanID == "" || result.Attempt <= 0 || result.EffectsJSON == "" || result.StartedAt.IsZero() || result.CompletedAt.IsZero() {
		return errors.New("invalid Setup Execution Result")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO setup_execution_results(plan_id,attempt,status,effects_json,diagnostic,started_at,completed_at) VALUES(?,?,?,?,?,?,?)`, result.PlanID, result.Attempt, result.Status, result.EffectsJSON, result.Diagnostic, formatTimestamp(result.StartedAt), formatTimestamp(result.CompletedAt))
	return err
}

func (s *Store) SetupExecutionResults(ctx context.Context, planID string) ([]SetupExecutionResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT result_id,plan_id,attempt,status,effects_json,diagnostic,started_at,completed_at FROM setup_execution_results WHERE plan_id=? ORDER BY attempt`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SetupExecutionResult
	for rows.Next() {
		var value SetupExecutionResult
		var started, completed string
		if err := rows.Scan(&value.ResultID, &value.PlanID, &value.Attempt, &value.Status, &value.EffectsJSON, &value.Diagnostic, &started, &completed); err != nil {
			return nil, err
		}
		value.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		value.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed)
		results = append(results, value)
	}
	return results, rows.Err()
}

func (s *Store) LatestSetupPlan(ctx context.Context, kind string) (SetupPlanRecord, error) {
	if kind == "" {
		return SetupPlanRecord{}, errors.New("Setup Plan kind is required")
	}
	plan, err := scanSetupPlan(s.db.QueryRowContext(ctx, `SELECT plan_id,kind,schema_version,target,digest_sha256,canonical_json,projection,created_at FROM setup_plans WHERE kind=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return SetupPlanRecord{}, ErrNotFound
	}
	return plan, err
}

func scanSetupPlan(row *sql.Row) (SetupPlanRecord, error) {
	var p SetupPlanRecord
	var created string
	err := row.Scan(&p.PlanID, &p.Kind, &p.SchemaVersion, &p.Target, &p.DigestSHA256, &p.CanonicalJSON, &p.Projection, &created)
	if err == nil {
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	}
	return p, err
}
