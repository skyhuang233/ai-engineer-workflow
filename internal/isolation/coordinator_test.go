package isolation

import (
	"context"
	"testing"

	"github.com/skyhuang233/workflow/internal/store"
)

type coordinatorStore struct {
	cancel       context.CancelFunc
	acknowledged bool
}

func (s *coordinatorStore) FenceDeliveryIsolation(_ context.Context, targets []store.TicketClaim) ([]store.TicketClaim, error) {
	s.cancel()
	return targets, nil
}

func (s *coordinatorStore) AcknowledgeDeliveryIsolation(ctx context.Context, targets []store.TicketClaim) ([]store.DeliveryIsolationProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.acknowledged = true
	return make([]store.DeliveryIsolationProof, len(targets)), nil
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

func TestDeliveryControllersCompletesHandshakeAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	database := &coordinatorStore{cancel: cancel}
	isolator := &coordinatorIsolator{}
	targets := []store.TicketClaim{{RunID: "delivery-1"}}

	acknowledged, err := DeliveryControllers(ctx, database, isolator, targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolator.isolated) != 1 || !database.acknowledged || len(acknowledged) != 1 {
		t.Fatalf("isolation handshake incomplete: isolated=%v acknowledged=%t proofs=%#v", isolator.isolated, database.acknowledged, acknowledged)
	}
}

func TestRetryDeliveryControllerTransitionAccumulatesConcurrentIsolation(t *testing.T) {
	database := &coordinatorStore{cancel: func() {}}
	isolator := &coordinatorIsolator{}
	targets := []store.TicketClaim{{RunID: "delivery-1"}, {RunID: "delivery-2"}}
	calls := 0

	err := RetryDeliveryControllerTransition(context.Background(), database, isolator, func(isolated []store.DeliveryIsolationProof) error {
		calls++
		if len(isolated) < len(targets) {
			return &store.DeliveryIsolationRequired{Targets: []store.TicketClaim{targets[len(isolated)]}}
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
