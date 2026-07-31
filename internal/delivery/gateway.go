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
	if _, err := g.Store.ValidateDelivery(ctx, outbox.Request, g.now()); err != nil {
		_ = g.Store.FinishDeliveryOutbox(ctx, key, store.OutboxRejected, err.Error(), g.now())
		return err
	}
	if outbox.Request.ExpectedRemoteHead != "" {
		observation, observeErr := g.Remote.Observe(ctx, outbox.Request)
		if observeErr != nil {
			return g.retry(ctx, key, observeErr)
		}
		if observation.RemoteHead != outbox.Request.ExpectedRemoteHead {
			err := fmt.Errorf("%w: remote head %q does not match expected %q", store.ErrDeliveryRejected, observation.RemoteHead, outbox.Request.ExpectedRemoteHead)
			_ = g.Store.RecordDeliveryAudit(ctx, outbox.Request, "rejected", err.Error(), g.now())
			_ = g.Store.FinishDeliveryOutbox(ctx, key, store.OutboxRejected, err.Error(), g.now())
			return err
		}
	}
	observation, applyErr := g.Remote.Apply(ctx, outbox.Request)
	if applyErr == nil {
		return g.succeed(ctx, outbox, observation)
	}
	// A timeout, connection reset, or process crash can leave the remote write
	// committed. The only safe decision is a read-after-error reconciliation.
	observed, observeErr := g.Remote.Observe(ctx, outbox.Request)
	if observeErr == nil && observed.Applied {
		return g.succeed(ctx, outbox, observed)
	}
	if observeErr != nil {
		applyErr = fmt.Errorf("%v; observe uncertain result: %w", applyErr, observeErr)
	}
	return g.retry(ctx, key, applyErr)
}

func (g Gateway) succeed(ctx context.Context, outbox store.DeliveryOutbox, observation Observation) error {
	if observation.PullRequestNumber != 0 && (outbox.Request.Operation == store.DeliveryUpsertPR || outbox.Request.Operation == store.DeliveryReplyEvidence) {
		if err := g.Store.RecordDeliveryMapping(ctx, outbox.Request, observation.PullRequestNumber, observation.PullRequestNodeID, observation.RemoteHead, g.now()); err != nil {
			_ = g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, store.OutboxRejected, err.Error(), g.now())
			return err
		}
	}
	return g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, store.OutboxSucceeded, "", g.now())
}

func (g Gateway) retry(ctx context.Context, key string, err error) error {
	_ = g.Store.FinishDeliveryOutbox(ctx, key, store.OutboxPending, err.Error(), g.now())
	return err
}
