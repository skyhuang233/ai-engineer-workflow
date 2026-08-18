package store

import "context"

// NextSetupExecutionAttempt reserves the next immutable retry ordinal for one
// exact Onboarding Plan. Execution results are append-only; a retry must never
// overwrite the incomplete attempt that created a pending Pull Request.
func (s *Store) NextSetupExecutionAttempt(ctx context.Context, planID string) (int, error) {
	var next int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt),0)+1 FROM setup_execution_results WHERE plan_id=?`, planID).Scan(&next)
	if err != nil {
		return 0, err
	}
	return next, nil
}
