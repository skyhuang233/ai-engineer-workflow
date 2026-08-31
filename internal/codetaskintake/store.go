// Package codetaskintake owns durable, idempotent task acceptance. Execution
// consumes the returned task reference in a separate capability.
package codetaskintake

import (
	"context"
	"encoding/json"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/store"
)

type StoreIntake struct {
	Store *store.Store
	Now   func() time.Time
}

func (i StoreIntake) AcceptIssue(ctx context.Context, repository string, issue controlplane.ObservedIssue) (string, error) {
	now := i.Now
	if now == nil {
		now = time.Now
	}
	snapshot, err := json.Marshal(issue)
	if err != nil {
		return "", err
	}
	receipt, _, err := i.Store.AcceptCodeTaskIssue(ctx, store.CodeTaskReceipt{Repository: repository, GitHubIssueID: issue.ID, TaskReference: store.CodeTaskReference(repository, issue.ID), SnapshotJSON: string(snapshot), AcceptedAt: now().UTC()})
	if err != nil {
		return "", err
	}
	return receipt.TaskReference, nil
}

var _ controlplane.CodeTaskIntake = StoreIntake{}
