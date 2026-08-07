// Package delivery contains the narrow, lease-fenced boundary between a
// Ticket Agent's schema commands and external repository mutations.
package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

var (
	ErrGatewayCredentialRejected = errors.New("Gateway Credential was rejected")
	ErrGatewayWritesPaused       = errors.New("Gateway writes are paused")
	ErrGatewayStore              = errors.New("Gateway persistence is temporarily unavailable")
)

const (
	controlPlaneRemoteTimeout = time.Minute
	outboxCleanupTimeout      = 5 * time.Second
)

type authenticationFailure interface {
	AuthenticationFailure() bool
}

type retryAtFailure interface {
	RetryAtTime() time.Time
}

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

type credentialAwareRemote interface {
	CredentialAvailable(context.Context) error
}

type gatewayStore interface {
	EnqueueDelivery(context.Context, store.DeliveryRequest, time.Time) (store.DeliveryOutbox, error)
	EnsureGatewayDispatcher(context.Context, string, time.Time) error
	ClaimDeliveryOutboxForDispatcher(context.Context, string, string, time.Time) (store.DeliveryOutbox, error)
	ExecuteDelivery(context.Context, store.DeliveryRequest, string, func() time.Time, func(context.Context, store.DeliveryRequest) (store.DeliveryResult, error)) (store.DeliveryResult, error)
	PlanProjectionAt(context.Context, string, time.Time) (plan.Projection, error)
	QueueWorkflowInboxProjection(context.Context, string, time.Time) (store.DeliveryOutbox, error)
	QueueWorkflowInboxProjectionIfActive(context.Context, string, time.Time) (store.DeliveryOutbox, error)
	DeliveryOutbox(context.Context, string) (store.DeliveryOutbox, error)
	DueDeliveryOutboxKeys(context.Context, time.Time, int) ([]string, error)
	GatewayCredentialAttentionRepositories(context.Context) ([]string, error)
	RenewGatewayDispatcher(context.Context, string, time.Time) error
	RequeueDeliveryOutboxClaim(context.Context, string, string, string, bool, time.Time) error
	CompleteDeliveryOutbox(context.Context, string, string, store.DeliveryResult, time.Time) error
	MarkDeliveryOutboxUncertain(context.Context, string, string, string, time.Time) error
	RecordDeliveryAudit(context.Context, store.DeliveryRequest, string, string, time.Time) error
	FinishDeliveryOutbox(context.Context, string, string, string, string, time.Time) error
	DeferDeliveryOutbox(context.Context, string, string, string, bool, time.Time, time.Time) error
	PauseGatewayWrites(context.Context, string, time.Time) error
}

type Gateway struct {
	Store           gatewayStore
	Remote          Remote
	Now             func() time.Time
	DispatcherToken string
}

func NewGateway(store *store.Store, remote Remote) (Gateway, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return Gateway{}, err
	}
	return Gateway{Store: store, Remote: remote, DispatcherToken: "gateway-dispatcher-" + hex.EncodeToString(bytes)}, nil
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
	outbox, err := g.Store.EnqueueDelivery(ctx, request, g.now())
	if err != nil && !errors.Is(err, store.ErrDeliveryRejected) && !errors.Is(err, store.ErrInvalidClaim) && !errors.Is(err, store.ErrFencingConflict) {
		return store.DeliveryOutbox{}, errors.Join(ErrGatewayStore, err)
	}
	return outbox, err
}

// Dispatch claims one outbox item and executes it. An external error remains
// retryable unless an observation proves that the requested mutation already
// happened.
func (g Gateway) Dispatch(ctx context.Context, key string) error {
	if g.Store == nil || g.Remote == nil {
		return errors.New("delivery gateway dependencies are incomplete")
	}
	dispatcherToken := g.DispatcherToken
	if dispatcherToken == "" {
		dispatcherToken = "legacy-gateway-dispatcher"
	}
	if err := g.Store.EnsureGatewayDispatcher(ctx, dispatcherToken, g.now()); err != nil {
		return err
	}
	outbox, err := g.Store.ClaimDeliveryOutboxForDispatcher(ctx, key, dispatcherToken, g.now())
	if err != nil {
		if errors.Is(err, store.ErrGatewayWritesPaused) {
			return fmt.Errorf("%w: %v", ErrGatewayWritesPaused, err)
		}
		if errors.Is(err, store.ErrInboxDeliveryPending) {
			queued, loadErr := g.Store.DeliveryOutbox(ctx, key)
			if loadErr == nil && queued.Request.Operation == store.DeliveryProjectInbox {
				return nil
			}
		}
		return err
	}
	if outbox.State == store.OutboxSucceeded {
		return nil
	}
	if outbox.State == store.OutboxRejected {
		return fmt.Errorf("%w: %s", store.ErrDeliveryRejected, outbox.LastError)
	}
	operationCtx, stopDispatcher, err := g.controlPlaneDispatchContext(ctx, outbox, dispatcherToken)
	if err != nil {
		return errors.Join(err, g.requeueClaim(outbox, err, false))
	}
	if remote, ok := g.Remote.(credentialAwareRemote); ok {
		if err := g.credentialAvailable(operationCtx, outbox.Request, remote); err != nil {
			if leaseErr := stopDispatcher(); leaseErr != nil {
				return errors.Join(err, leaseErr, g.requeueClaim(outbox, leaseErr, false))
			}
			if isCredentialRejection(err) {
				return g.pauseForCredential(outbox, err)
			}
			return g.retry(outbox, err)
		}
	}
	if outbox.ReconcileOnly {
		if outbox.Request.Operation == store.DeliveryProjectInbox {
			result, err := g.Store.ExecuteDelivery(operationCtx, outbox.Request, outbox.ClaimToken, g.now, func(operationCtx context.Context, request store.DeliveryRequest) (store.DeliveryResult, error) {
				observation, observeErr := g.observe(operationCtx, request)
				if observeErr != nil {
					return store.DeliveryResult{}, observeErr
				}
				if !observation.Applied {
					return store.DeliveryResult{}, fmt.Errorf("%w: uncertain Workflow Inbox delivery was not observed", store.ErrDeliveryRejected)
				}
				return resultFrom(observation), nil
			})
			if leaseErr := stopDispatcher(); leaseErr != nil {
				return errors.Join(err, leaseErr, g.requeueClaim(outbox, leaseErr, true))
			}
			if err != nil {
				if isCredentialRejection(err) {
					return g.pauseForCredential(outbox, err)
				}
				if errors.Is(err, store.ErrDeliverySuperseded) {
					return g.succeed(outbox, store.DeliveryResult{})
				}
				if errors.Is(err, store.ErrDeliveryRejected) {
					return g.reject(outbox, err)
				}
				return errors.Join(err, g.markUncertain(outbox, err))
			}
			return g.succeed(outbox, result)
		}
		err := g.reconcileOnly(operationCtx, outbox)
		if leaseErr := stopDispatcher(); leaseErr != nil {
			return errors.Join(err, leaseErr, g.requeueClaim(outbox, leaseErr, true))
		}
		return err
	}
	result, err := g.Store.ExecuteDelivery(operationCtx, outbox.Request, outbox.ClaimToken, g.now, func(operationCtx context.Context, request store.DeliveryRequest) (store.DeliveryResult, error) {
		if request.Operation == store.DeliveryProjectPlan {
			projection, projectionErr := g.Store.PlanProjectionAt(operationCtx, request.PlanProjection.VersionID, g.now())
			if projectionErr != nil {
				return store.DeliveryResult{}, errors.Join(ErrGatewayStore, projectionErr)
			}
			request.PlanProjection = &projection
		}
		observation, observeErr := g.observe(operationCtx, request)
		if observeErr != nil {
			return store.DeliveryResult{}, observeErr
		}
		if observation.Applied {
			return resultFrom(observation), nil
		}
		if request.ExpectRemoteAbsent && observation.RemoteExists {
			return store.DeliveryResult{}, fmt.Errorf("%w: remote branch already exists at %q", store.ErrDeliveryRejected, observation.RemoteHead)
		}
		if request.ExpectedRemoteHead != "" && (!observation.RemoteExists || observation.RemoteHead != request.ExpectedRemoteHead) {
			return store.DeliveryResult{}, fmt.Errorf("%w: remote head %q does not match expected %q", store.ErrDeliveryRejected, observation.RemoteHead, request.ExpectedRemoteHead)
		}
		observation, applyErr := g.apply(operationCtx, request)
		if applyErr != nil {
			if retryAt(applyErr).After(g.now()) {
				return store.DeliveryResult{}, applyErr
			}
			observed, observeErr := g.observe(operationCtx, request)
			if observeErr == nil && observed.Applied {
				observation = observed
				applyErr = nil
			} else {
				applyErr = &uncertainWriteError{applyErr: applyErr, observeErr: observeErr}
			}
		}
		if applyErr != nil {
			return store.DeliveryResult{}, applyErr
		}
		return store.DeliveryResult{RemoteHead: observation.RemoteHead, PullRequestNumber: observation.PullRequestNumber, PullRequestNodeID: observation.PullRequestNodeID}, nil
	})
	if leaseErr := stopDispatcher(); leaseErr != nil {
		return errors.Join(err, leaseErr, g.requeueClaim(outbox, leaseErr, true))
	}
	if err != nil {
		if isCredentialRejection(err) {
			return g.pauseForCredential(outbox, err)
		}
		if errors.Is(err, store.ErrDeliverySuperseded) {
			return g.succeed(outbox, store.DeliveryResult{})
		}
		if errors.Is(err, store.ErrDeliveryRejected) {
			return g.reject(outbox, err)
		}
		if errors.Is(err, store.ErrDeliveryUncertain) {
			if finishErr := g.markUncertain(outbox, err); finishErr != nil {
				return errors.Join(err, finishErr)
			}
			return err
		}
		var uncertain *uncertainWriteError
		if errors.As(err, &uncertain) {
			if finishErr := g.markUncertain(outbox, err); finishErr != nil {
				return errors.Join(err, finishErr)
			}
			return err
		}
		return g.retry(outbox, err)
	}
	return g.succeed(outbox, result)
}

func (g Gateway) DispatchPending(ctx context.Context, limit int) error {
	if g.Store == nil || g.Remote == nil {
		return errors.New("delivery gateway dependencies are incomplete")
	}
	var dispatchErr error
	for dispatched := 0; dispatched < limit; {
		keys, err := g.Store.DueDeliveryOutboxKeys(ctx, g.now(), limit-dispatched)
		if err != nil {
			return errors.Join(dispatchErr, err)
		}
		if len(keys) == 0 {
			return dispatchErr
		}
		progressed := false
		for _, key := range keys {
			dispatched++
			err := g.Dispatch(ctx, key)
			if errors.Is(err, store.ErrDeliveryInProgress) {
				continue
			}
			progressed = true
			if err != nil {
				dispatchErr = errors.Join(dispatchErr, err)
			}
		}
		if !progressed {
			return dispatchErr
		}
	}
	return dispatchErr
}

func (g Gateway) QueueGatewayCredentialInboxProjections(ctx context.Context) error {
	if g.Store == nil {
		return errors.New("delivery gateway store is missing")
	}
	repositories, err := g.Store.GatewayCredentialAttentionRepositories(ctx)
	if err != nil {
		return err
	}
	for _, repository := range repositories {
		if _, err := g.Store.QueueWorkflowInboxProjectionIfActive(ctx, repository, g.now()); err != nil {
			return err
		}
	}
	return nil
}

func (g Gateway) reconcileOnly(ctx context.Context, outbox store.DeliveryOutbox) error {
	observation, err := g.observe(ctx, outbox.Request)
	if err != nil {
		if isCredentialRejection(err) {
			return g.pauseForCredential(outbox, err)
		}
		if finishErr := g.markUncertain(outbox, err); finishErr != nil {
			return errors.Join(err, finishErr)
		}
		return err
	}
	if observation.Applied {
		return g.succeed(outbox, resultFrom(observation))
	}
	return g.reject(outbox, fmt.Errorf("%w: uncertain delivery was not observed", store.ErrDeliveryRejected))
}

func (g Gateway) controlPlaneDispatchContext(ctx context.Context, outbox store.DeliveryOutbox, dispatcherToken string) (context.Context, func() error, error) {
	if outbox.Request.RunID != "" {
		return ctx, func() error { return nil }, nil
	}
	if err := g.renewDispatcher(dispatcherToken); err != nil {
		return nil, nil, err
	}
	operationCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan struct{})
	renewalErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(controlPlaneRemoteTimeout / 3)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-operationCtx.Done():
				return
			case <-ticker.C:
				if err := g.renewDispatcher(dispatcherToken); err != nil {
					renewalErr <- err
					cancel()
					return
				}
			}
		}
	}()
	return operationCtx, func() error {
		close(stop)
		cancel()
		<-done
		select {
		case err := <-renewalErr:
			return err
		default:
		}
		return g.renewDispatcher(dispatcherToken)
	}, nil
}

func (g Gateway) credentialAvailable(ctx context.Context, request store.DeliveryRequest, remote credentialAwareRemote) error {
	operationCtx, cancel := g.remoteContext(ctx, request)
	defer cancel()
	return remote.CredentialAvailable(operationCtx)
}

func (g Gateway) observe(ctx context.Context, request store.DeliveryRequest) (Observation, error) {
	operationCtx, cancel := g.remoteContext(ctx, request)
	defer cancel()
	return g.Remote.Observe(operationCtx, request)
}

func (g Gateway) apply(ctx context.Context, request store.DeliveryRequest) (Observation, error) {
	operationCtx, cancel := g.remoteContext(ctx, request)
	defer cancel()
	return g.Remote.Apply(operationCtx, request)
}

func (g Gateway) remoteContext(ctx context.Context, request store.DeliveryRequest) (context.Context, context.CancelFunc) {
	if request.RunID != "" {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, controlPlaneRemoteTimeout)
}

func resultFrom(observation Observation) store.DeliveryResult {
	return store.DeliveryResult{RemoteHead: observation.RemoteHead, PullRequestNumber: observation.PullRequestNumber, PullRequestNodeID: observation.PullRequestNodeID}
}

func (g Gateway) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), outboxCleanupTimeout)
}

func (g Gateway) renewDispatcher(dispatcherToken string) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	return g.Store.RenewGatewayDispatcher(ctx, dispatcherToken, g.now())
}

func (g Gateway) requeueClaim(outbox store.DeliveryOutbox, cause error, uncertain bool) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	err := g.Store.RequeueDeliveryOutboxClaim(ctx, outbox.IdempotencyKey, outbox.ClaimToken, cause.Error(), uncertain, g.now())
	if errors.Is(err, store.ErrFencingConflict) {
		return nil
	}
	return err
}

func (g Gateway) succeed(outbox store.DeliveryOutbox, result store.DeliveryResult) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	err := g.Store.CompleteDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, result, g.now())
	if err != nil {
		return errors.Join(err, g.requeueClaim(outbox, err, true))
	}
	return nil
}

func (g Gateway) markUncertain(outbox store.DeliveryOutbox, cause error) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	now := g.now()
	if retryAfter := retryAt(cause); retryAfter.After(now) {
		err := g.Store.DeferDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, cause.Error(), true, retryAfter, now)
		if err != nil {
			return errors.Join(err, g.requeueClaim(outbox, cause, true))
		}
		return nil
	}
	err := g.Store.MarkDeliveryOutboxUncertain(ctx, outbox.IdempotencyKey, outbox.ClaimToken, cause.Error(), now)
	if err != nil {
		return errors.Join(err, g.requeueClaim(outbox, cause, true))
	}
	return nil
}

func (g Gateway) reject(outbox store.DeliveryOutbox, cause error) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	_ = g.Store.RecordDeliveryAudit(ctx, outbox.Request, "rejected", cause.Error(), g.now())
	err := g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxRejected, cause.Error(), g.now())
	if err != nil {
		return errors.Join(cause, err, g.requeueClaim(outbox, cause, false))
	}
	return cause
}

func (g Gateway) retry(outbox store.DeliveryOutbox, cause error) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	now := g.now()
	var err error
	if retryAfter := retryAt(cause); retryAfter.After(now) {
		err = g.Store.DeferDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, cause.Error(), false, retryAfter, now)
	} else {
		err = g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxPending, cause.Error(), now)
	}
	if err != nil {
		return errors.Join(cause, err, g.requeueClaim(outbox, cause, false))
	}
	return cause
}

func (g Gateway) pauseForCredential(outbox store.DeliveryOutbox, cause error) error {
	ctx, cancel := g.cleanupContext()
	defer cancel()
	reason := "Gateway Credential was rejected; replace and verify it to resume writes"
	if err := g.Store.PauseGatewayWrites(ctx, reason, g.now()); err != nil {
		return errors.Join(fmt.Errorf("%v; persist Gateway pause: %w", cause, err), g.requeueClaim(outbox, cause, false))
	}
	if err := g.QueueGatewayCredentialInboxProjections(ctx); err != nil {
		return errors.Join(fmt.Errorf("%v; queue Workflow Inbox recovery request: %w", cause, err), g.requeueClaim(outbox, cause, false))
	}
	if err := g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxPending, reason, g.now()); err != nil {
		return errors.Join(fmt.Errorf("%w: %v", ErrGatewayWritesPaused, cause), err, g.requeueClaim(outbox, cause, false))
	}
	return fmt.Errorf("%w: %v", ErrGatewayWritesPaused, cause)
}

func isCredentialRejection(err error) bool {
	if errors.Is(err, ErrGatewayCredentialRejected) {
		return true
	}
	var failure authenticationFailure
	return errors.As(err, &failure) && failure.AuthenticationFailure()
}

func retryAt(err error) time.Time {
	var failure retryAtFailure
	if errors.As(err, &failure) {
		return failure.RetryAtTime()
	}
	return time.Time{}
}
