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
	result, err := g.Store.ExecuteDelivery(ctx, outbox.Request, g.now(), func(request store.DeliveryRequest) (store.DeliveryResult, error) {
		if outbox.ReconcileOnly {
			observation, observeErr := g.Remote.Observe(ctx, request)
			if observeErr != nil {
				return store.DeliveryResult{}, observeErr
			}
			if !observation.Applied {
				return store.DeliveryResult{}, errors.New("delivery retries exhausted without observing the requested mutation")
			}
			return store.DeliveryResult{RemoteHead: observation.RemoteHead, PullRequestNumber: observation.PullRequestNumber, PullRequestNodeID: observation.PullRequestNodeID}, nil
		}
		if request.ExpectedRemoteHead != "" || request.ExpectRemoteAbsent {
			observation, observeErr := g.Remote.Observe(ctx, request)
			if observeErr != nil {
				return store.DeliveryResult{}, observeErr
			}
			if request.ExpectRemoteAbsent && observation.RemoteExists {
				return store.DeliveryResult{}, fmt.Errorf("%w: remote branch already exists at %q", store.ErrDeliveryRejected, observation.RemoteHead)
			}
			if request.ExpectedRemoteHead != "" && (!observation.RemoteExists || observation.RemoteHead != request.ExpectedRemoteHead) {
				return store.DeliveryResult{}, fmt.Errorf("%w: remote head %q does not match expected %q", store.ErrDeliveryRejected, observation.RemoteHead, request.ExpectedRemoteHead)
			}
		}
		observation, applyErr := g.Remote.Apply(ctx, request)
		if applyErr != nil {
			observed, observeErr := g.Remote.Observe(ctx, request)
			if observeErr == nil && observed.Applied {
				observation = observed
				applyErr = nil
			} else if observeErr != nil {
				applyErr = fmt.Errorf("%v; observe uncertain result: %w", applyErr, observeErr)
			}
		}
		if applyErr != nil {
			return store.DeliveryResult{}, applyErr
		}
		return store.DeliveryResult{RemoteHead: observation.RemoteHead, PullRequestNumber: observation.PullRequestNumber, PullRequestNodeID: observation.PullRequestNodeID}, nil
	})
	if err != nil {
		if errors.Is(err, store.ErrDeliveryRejected) {
			_ = g.Store.RecordDeliveryAudit(ctx, outbox.Request, "rejected", err.Error(), g.now())
			finishErr := g.Store.FinishDeliveryOutbox(ctx, key, outbox.ClaimToken, store.OutboxRejected, err.Error(), g.now())
			if finishErr != nil {
				return finishErr
			}
			return err
		}
		return g.retry(ctx, outbox, err)
	}
	return g.succeed(ctx, outbox, result)
}

func (g Gateway) succeed(ctx context.Context, outbox store.DeliveryOutbox, _ store.DeliveryResult) error {
	return g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxSucceeded, "", g.now())
}

func (g Gateway) retry(ctx context.Context, outbox store.DeliveryOutbox, err error) error {
	if finishErr := g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxPending, err.Error(), g.now()); finishErr != nil {
		return finishErr
	}
	return err
}
