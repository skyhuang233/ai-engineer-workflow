package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CodeTaskReceipt is the idempotent acceptance record returned by the
// separate Code Task Intake capability. It deliberately has no Worker, branch,
// pull-request, review, or completion fields; those belong to task execution.
type CodeTaskReceipt struct {
	Repository    string
	GitHubIssueID int64
	TaskReference string
	SnapshotJSON  string
	AcceptedAt    time.Time
}

func (r CodeTaskReceipt) Validate() error {
	if !repositoryIdentityPattern.MatchString(r.Repository) || r.GitHubIssueID <= 0 || strings.TrimSpace(r.TaskReference) == "" || strings.TrimSpace(r.SnapshotJSON) == "" || r.AcceptedAt.IsZero() {
		return errors.New("invalid Code Task receipt")
	}
	return nil
}

func (s *Store) AcceptCodeTaskIssue(ctx context.Context, receipt CodeTaskReceipt) (CodeTaskReceipt, bool, error) {
	if err := receipt.Validate(); err != nil {
		return CodeTaskReceipt{}, false, err
	}
	receipt.Repository = strings.TrimSpace(receipt.Repository)
	receipt.AcceptedAt = receipt.AcceptedAt.UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO code_task_receipts(repository,github_issue_id,task_reference,snapshot_json,accepted_at)
VALUES(?,?,?,?,?) ON CONFLICT(repository,github_issue_id) DO NOTHING`, receipt.Repository, receipt.GitHubIssueID, receipt.TaskReference, receipt.SnapshotJSON, formatTimestamp(receipt.AcceptedAt))
	if err != nil {
		return CodeTaskReceipt{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return CodeTaskReceipt{}, false, err
	}
	actual, err := s.CodeTaskReceipt(ctx, receipt.Repository, receipt.GitHubIssueID)
	return actual, inserted == 1, err
}

func (s *Store) CodeTaskReceipt(ctx context.Context, repository string, issueID int64) (CodeTaskReceipt, error) {
	var receipt CodeTaskReceipt
	var accepted string
	err := s.db.QueryRowContext(ctx, `SELECT repository,github_issue_id,task_reference,snapshot_json,accepted_at
FROM code_task_receipts WHERE repository=? AND github_issue_id=?`, strings.TrimSpace(repository), issueID).Scan(&receipt.Repository, &receipt.GitHubIssueID, &receipt.TaskReference, &receipt.SnapshotJSON, &accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return CodeTaskReceipt{}, ErrNotFound
	}
	if err != nil {
		return CodeTaskReceipt{}, err
	}
	receipt.AcceptedAt, err = time.Parse(time.RFC3339Nano, accepted)
	if err != nil {
		return CodeTaskReceipt{}, err
	}
	return receipt, receipt.Validate()
}

func CodeTaskReference(repository string, issueID int64) string {
	return fmt.Sprintf("code-task:%s:%d", repository, issueID)
}
