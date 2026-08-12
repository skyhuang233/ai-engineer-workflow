package isolation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

const defaultTimeout = 30 * time.Second

type Store interface {
	FenceDeliveryIsolation(context.Context, []store.TicketClaim) ([]store.TicketClaim, error)
	AcknowledgeDeliveryIsolation(context.Context, []store.TicketClaim) ([]store.DeliveryIsolationProof, error)
}

type ContainerIsolator interface {
	IsolateContainer(context.Context, string) error
}

func RetryDeliveryControllerTransition(ctx context.Context, database Store, isolator ContainerIsolator, transition func([]store.DeliveryIsolationProof) error) error {
	if transition == nil {
		return errors.New("Delivery Controller transition is required")
	}
	var isolated []store.DeliveryIsolationProof
	for {
		err := transition(isolated)
		var required *store.DeliveryIsolationRequired
		if !errors.As(err, &required) {
			return err
		}
		acknowledged, isolationErr := DeliveryControllers(ctx, database, isolator, required.Targets)
		if isolationErr != nil {
			return errors.Join(err, isolationErr)
		}
		isolated = append(isolated, acknowledged...)
	}
}

func DeliveryControllers(ctx context.Context, database Store, isolator ContainerIsolator, targets []store.TicketClaim) ([]store.DeliveryIsolationProof, error) {
	if database == nil || isolator == nil {
		return nil, errors.New("Delivery Controller isolation dependencies are incomplete")
	}
	fenced, err := database.FenceDeliveryIsolation(ctx, targets)
	if err != nil {
		return nil, fmt.Errorf("fence Delivery Controller isolation: %w", err)
	}
	persistenceCtx := context.WithoutCancel(ctx)
	for _, target := range fenced {
		isolationCtx, cancel := context.WithTimeout(persistenceCtx, defaultTimeout)
		isolateErr := isolator.IsolateContainer(isolationCtx, target.RunID)
		cancel()
		if isolateErr != nil {
			return nil, fmt.Errorf("isolate Delivery Controller %s: %w", target.RunID, isolateErr)
		}
	}
	acknowledgeCtx, cancel := context.WithTimeout(persistenceCtx, defaultTimeout)
	acknowledged, err := database.AcknowledgeDeliveryIsolation(acknowledgeCtx, fenced)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("acknowledge Delivery Controller isolation: %w", err)
	}
	return acknowledged, nil
}
