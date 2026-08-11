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

func (s *coordinatorStore) AcknowledgeDeliveryIsolation(ctx context.Context, targets []store.TicketClaim) ([]store.TicketClaim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.acknowledged = true
	return targets, nil
}

type coordinatorIsolator struct {
	isolated bool
}

func (i *coordinatorIsolator) IsolateContainer(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.isolated = true
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
	if !isolator.isolated || !database.acknowledged || len(acknowledged) != 1 {
		t.Fatalf("isolation handshake incomplete: isolated=%t acknowledged=%t proofs=%#v", isolator.isolated, database.acknowledged, acknowledged)
	}
}
