package isolation

import (
	"context"
	"testing"

	"github.com/skyhuang233/workflow/internal/store"
)

type coordinatorStore struct {
	cancel       context.CancelFunc
	acknowledged bool
	fenceErrors  []error
}

func (s *coordinatorStore) FenceWorkerIsolation(_ context.Context, targets []store.TicketClaim) ([]store.TicketClaim, error) {
	if len(s.fenceErrors) > 0 {
		err := s.fenceErrors[0]
		s.fenceErrors = s.fenceErrors[1:]
		return nil, err
	}
	s.cancel()
	return targets, nil
}

func TestRetryWorkerTransitionReplaysAfterStaleIsolationTarget(t *testing.T) {
	database := &coordinatorStore{cancel: func() {}, fenceErrors: []error{store.ErrFencingConflict}}
	isolator := &coordinatorIsolator{}
	target := store.TicketClaim{RunID: "stale-worker"}
	calls := 0

	err := RetryWorkerTransition(context.Background(), database, isolator, func([]store.WorkerIsolationProof) error {
		calls++
		if calls == 1 {
			return &store.WorkerIsolationRequired{Targets: []store.TicketClaim{target}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(isolator.isolated) != 0 {
		t.Fatalf("transition calls = %d, isolated = %v", calls, isolator.isolated)
	}
}

func (s *coordinatorStore) AcknowledgeWorkerIsolation(ctx context.Context, targets []store.TicketClaim) ([]store.WorkerIsolationProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.acknowledged = true
	return make([]store.WorkerIsolationProof, len(targets)), nil
}

type coordinatorIsolator struct {
	isolated []string
}

func (i *coordinatorIsolator) IsolateContainer(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.isolated = append(i.isolated, runID)
	return nil
}

func TestIsolateWorkersCompletesHandshakeAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	database := &coordinatorStore{cancel: cancel}
	isolator := &coordinatorIsolator{}
	targets := []store.TicketClaim{{RunID: "delivery-1"}}

	acknowledged, err := IsolateWorkers(ctx, database, isolator, targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolator.isolated) != 1 || !database.acknowledged || len(acknowledged) != 1 {
		t.Fatalf("isolation handshake incomplete: isolated=%v acknowledged=%t proofs=%#v", isolator.isolated, database.acknowledged, acknowledged)
	}
}

func TestRetryWorkerTransitionAccumulatesConcurrentIsolation(t *testing.T) {
	database := &coordinatorStore{cancel: func() {}}
	isolator := &coordinatorIsolator{}
	targets := []store.TicketClaim{{RunID: "delivery-1"}, {RunID: "delivery-2"}}
	calls := 0

	err := RetryWorkerTransition(context.Background(), database, isolator, func(isolated []store.WorkerIsolationProof) error {
		calls++
		if len(isolated) < len(targets) {
			return &store.WorkerIsolationRequired{Targets: []store.TicketClaim{targets[len(isolated)]}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(isolator.isolated) != 2 || !database.acknowledged {
		t.Fatalf("transition retries = %d, isolated = %v, acknowledged = %t", calls, isolator.isolated, database.acknowledged)
	}
}
