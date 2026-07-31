// Package delivery contains the narrow, lease-fenced boundary between a
// Ticket Agent's schema commands and external repository mutations.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type Observation struct {
	Applied           bool
	RemoteHead        string
	RemoteExists      bool
	PullRequestNumber int64
	PullRequestNodeID string
}

// Remote is deliberately command-shaped. Implementations own GitHub API and
// git details; the gateway owns authorization, idempotency, and uncertainty.
type Remote interface {
	Observe(context.Context, store.DeliveryRequest) (Observation, error)
	Apply(context.Context, store.DeliveryRequest) (Observation, error)
}

type Gateway struct {
	Store  *store.Store
	Remote Remote
	Now    func() time.Time
}

type uncertainWriteError struct {
	applyErr   error
	observeErr error
}

func (e *uncertainWriteError) Error() string {
	return fmt.Sprintf("%v; observe uncertain result: %v", e.applyErr, e.observeErr)
}

func (e *uncertainWriteError) Unwrap() error {
	return e.applyErr
}

func (g Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// Submit performs the durable admission step. It never calls GitHub.
func (g Gateway) Submit(ctx context.Context, request store.DeliveryRequest) (store.DeliveryOutbox, error) {
	if g.Store == nil {
		return store.DeliveryOutbox{}, errors.New("delivery gateway store is missing")
	}
	return g.Store.EnqueueDelivery(ctx, request, g.now())
}

// Dispatch claims one outbox item and executes it. An external error remains
// retryable unless an observation proves that the requested mutation already
// happened. This is the important timeout rule: observe first, then retry.
func (g Gateway) Dispatch(ctx context.Context, key string) error {
	if g.Store == nil || g.Remote == nil {
		return errors.New("delivery gateway dependencies are incomplete")
	}
	outbox, err := g.Store.ClaimDeliveryOutbox(ctx, key, g.now())
	if err != nil {
		return err
	}
	if outbox.State == store.OutboxSucceeded {
		return nil
	}
	if outbox.State == store.OutboxRejected {
		return fmt.Errorf("%w: %s", store.ErrDeliveryRejected, outbox.LastError)
	}
	observation, observeErr := g.Remote.Observe(ctx, outbox.Request)
	if observeErr != nil {
		if outbox.ReconcileOnly || outbox.Uncertain {
			if finishErr := g.Store.MarkDeliveryOutboxUncertain(ctx, outbox.IdempotencyKey, outbox.ClaimToken, observeErr.Error(), g.now()); finishErr != nil {
				return finishErr
			}
			return observeErr
		}
		return g.retry(ctx, outbox, observeErr)
	}
	if observation.Applied {
		return g.succeed(ctx, outbox, resultFrom(observation))
	}
	if outbox.Request.ExpectRemoteAbsent && observation.RemoteExists {
		err := fmt.Errorf("%w: remote branch already exists at %q", store.ErrDeliveryRejected, observation.RemoteHead)
		return g.reject(ctx, outbox, err)
	}
	if outbox.Request.ExpectedRemoteHead != "" && (!observation.RemoteExists || observation.RemoteHead != outbox.Request.ExpectedRemoteHead) {
		err := fmt.Errorf("%w: remote head %q does not match expected %q", store.ErrDeliveryRejected, observation.RemoteHead, outbox.Request.ExpectedRemoteHead)
		return g.reject(ctx, outbox, err)
	}
	result, err := g.Store.ExecuteDelivery(ctx, outbox.Request, g.now(), func(operationCtx context.Context, request store.DeliveryRequest) (store.DeliveryResult, error) {
		observation, applyErr := g.Remote.Apply(operationCtx, request)
		if applyErr != nil {
			observed, observeErr := g.Remote.Observe(ctx, request)
			if observeErr == nil && observed.Applied {
				observation = observed
				applyErr = nil
			} else if observeErr != nil {
				applyErr = &uncertainWriteError{applyErr: applyErr, observeErr: observeErr}
			}
		}
		if applyErr != nil {
			return store.DeliveryResult{}, applyErr
		}
		return store.DeliveryResult{RemoteHead: observation.RemoteHead, PullRequestNumber: observation.PullRequestNumber, PullRequestNodeID: observation.PullRequestNodeID}, nil
	})
	if err != nil {
		if errors.Is(err, store.ErrDeliveryRejected) {
			return g.reject(ctx, outbox, err)
		}
		var uncertain *uncertainWriteError
		if errors.As(err, &uncertain) {
			if finishErr := g.Store.MarkDeliveryOutboxUncertain(ctx, outbox.IdempotencyKey, outbox.ClaimToken, err.Error(), g.now()); finishErr != nil {
				return finishErr
			}
			return err
		}
		return g.retry(ctx, outbox, err)
	}
	return g.succeed(ctx, outbox, result)
}

func resultFrom(observation Observation) store.DeliveryResult {
	return store.DeliveryResult{RemoteHead: observation.RemoteHead, PullRequestNumber: observation.PullRequestNumber, PullRequestNodeID: observation.PullRequestNodeID}
}

func (g Gateway) succeed(ctx context.Context, outbox store.DeliveryOutbox, result store.DeliveryResult) error {
	return g.Store.CompleteDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, result, g.now())
}

func (g Gateway) reject(ctx context.Context, outbox store.DeliveryOutbox, err error) error {
	_ = g.Store.RecordDeliveryAudit(ctx, outbox.Request, "rejected", err.Error(), g.now())
	if finishErr := g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxRejected, err.Error(), g.now()); finishErr != nil {
		return finishErr
	}
	return err
}

func (g Gateway) retry(ctx context.Context, outbox store.DeliveryOutbox, err error) error {
	if finishErr := g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxPending, err.Error(), g.now()); finishErr != nil {
		return finishErr
	}
	return err
}
