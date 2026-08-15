package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

type RepositoryCreateAttemptEvent string

const (
	RepositoryCreateStarted           RepositoryCreateAttemptEvent = "started"
	RepositoryCreateOutcomeUnknown    RepositoryCreateAttemptEvent = "outcome_unknown"
	RepositoryCreateSucceeded         RepositoryCreateAttemptEvent = "succeeded"
	RepositoryCreateDefinitiveFailure RepositoryCreateAttemptEvent = "definitive_failure"
)

// SetupRepositoryCreateAttemptEvent is append-only evidence for the narrow
// create_repository uncertainty window. It binds one external call to the
// immutable approved plan and exact repository identity that was absent when
// the plan was produced.
type SetupRepositoryCreateAttemptEvent struct {
	EventID                  int64
	PlanID                   string
	PlanDigestSHA256         string
	EffectID                 string
	ExecutionAttempt         int
	Event                    RepositoryCreateAttemptEvent
	Owner                    string
	Name                     string
	Private                  bool
	ApprovalAbsentRepository string
	RecordedAt               time.Time
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

func (s *Store) AppendSetupRepositoryCreateAttemptEvent(ctx context.Context, value SetupRepositoryCreateAttemptEvent) error {
	approvedRepository := strings.TrimSpace(value.Owner) + "/" + strings.TrimSpace(value.Name)
	if value.PlanID == "" || !fingerprintPattern.MatchString(value.PlanDigestSHA256) || value.EffectID == "" || value.ExecutionAttempt <= 0 || value.RecordedAt.IsZero() ||
		strings.Contains(value.Owner, "/") || strings.Contains(value.Name, "/") || strings.TrimSpace(value.Owner) == "" || strings.TrimSpace(value.Name) == "" || value.ApprovalAbsentRepository != approvedRepository ||
		(value.Event != RepositoryCreateStarted && value.Event != RepositoryCreateOutcomeUnknown && value.Event != RepositoryCreateSucceeded && value.Event != RepositoryCreateDefinitiveFailure) {
		return errors.New("invalid Setup repository-create attempt event")
	}
	var digest string
	if err := s.db.QueryRowContext(ctx, `SELECT digest_sha256 FROM setup_plans WHERE plan_id=?`, value.PlanID).Scan(&digest); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if digest != value.PlanDigestSHA256 {
		return ErrSetupPlanConflict
	}
	private := 0
	if value.Private {
		private = 1
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO setup_repository_create_attempt_events(plan_id,plan_digest_sha256,effect_id,execution_attempt,event,owner,name,private,approval_absent_repository,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(plan_id,effect_id,execution_attempt,event) DO NOTHING`,
		value.PlanID, value.PlanDigestSHA256, value.EffectID, value.ExecutionAttempt, string(value.Event), value.Owner, value.Name, private, value.ApprovalAbsentRepository, formatTimestamp(value.RecordedAt))
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 1 {
		return nil
	}
	var existing SetupRepositoryCreateAttemptEvent
	var existingPrivate int
	err = s.db.QueryRowContext(ctx, `SELECT plan_digest_sha256,owner,name,private,approval_absent_repository FROM setup_repository_create_attempt_events WHERE plan_id=? AND effect_id=? AND execution_attempt=? AND event=?`,
		value.PlanID, value.EffectID, value.ExecutionAttempt, string(value.Event)).Scan(&existing.PlanDigestSHA256, &existing.Owner, &existing.Name, &existingPrivate, &existing.ApprovalAbsentRepository)
	if err != nil {
		return err
	}
	if existing.PlanDigestSHA256 != value.PlanDigestSHA256 || existing.Owner != value.Owner || existing.Name != value.Name || (existingPrivate == 1) != value.Private || existing.ApprovalAbsentRepository != value.ApprovalAbsentRepository {
		return ErrSetupPlanConflict
	}
	return nil
}

func (s *Store) SetupRepositoryCreateAttemptEvents(ctx context.Context, planID, effectID string) ([]SetupRepositoryCreateAttemptEvent, error) {
	if planID == "" || effectID == "" {
		return nil, errors.New("Setup repository-create attempt identity is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,plan_id,plan_digest_sha256,effect_id,execution_attempt,event,owner,name,private,approval_absent_repository,recorded_at FROM setup_repository_create_attempt_events WHERE plan_id=? AND effect_id=? ORDER BY execution_attempt,event_id`, planID, effectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []SetupRepositoryCreateAttemptEvent
	for rows.Next() {
		var value SetupRepositoryCreateAttemptEvent
		var event, recorded string
		var private int
		if err := rows.Scan(&value.EventID, &value.PlanID, &value.PlanDigestSHA256, &value.EffectID, &value.ExecutionAttempt, &event, &value.Owner, &value.Name, &private, &value.ApprovalAbsentRepository, &recorded); err != nil {
			return nil, err
		}
		value.Event = RepositoryCreateAttemptEvent(event)
		value.Private = private == 1
		value.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded)
		if err != nil {
			return nil, fmt.Errorf("parse Setup repository-create attempt timestamp: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
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

func (s *Store) SetupPlanByDigest(ctx context.Context, digest string) (SetupPlanRecord, error) {
	if !fingerprintPattern.MatchString(digest) {
		return SetupPlanRecord{}, errors.New("Setup Plan digest is invalid")
	}
	plan, err := scanSetupPlan(s.db.QueryRowContext(ctx, `SELECT plan_id,kind,schema_version,target,digest_sha256,canonical_json,projection,created_at FROM setup_plans WHERE digest_sha256=?`, digest))
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
