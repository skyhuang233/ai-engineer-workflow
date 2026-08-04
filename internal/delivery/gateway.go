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
)

const controlPlaneRemoteTimeout = time.Minute

type authenticationFailure interface {
	AuthenticationFailure() bool
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

type Gateway struct {
	Store           *store.Store
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
	return g.Store.EnqueueDelivery(ctx, request, g.now())
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
		return err
	}
	if remote, ok := g.Remote.(credentialAwareRemote); ok {
		if err := g.credentialAvailable(operationCtx, outbox.Request, remote); err != nil {
			_ = stopDispatcher()
			return g.pauseForCredential(ctx, outbox, err)
		}
	}
	if outbox.ReconcileOnly {
		err := g.reconcileOnly(operationCtx, outbox)
		return errors.Join(err, stopDispatcher())
	}
	result, err := g.Store.ExecuteDelivery(operationCtx, outbox.Request, g.now, func(operationCtx context.Context, request store.DeliveryRequest) (store.DeliveryResult, error) {
		if request.Operation == store.DeliveryProjectPlan {
			projection, projectionErr := g.Store.PlanProjectionAt(operationCtx, request.PlanProjection.VersionID, g.now())
			if projectionErr != nil {
				return store.DeliveryResult{}, projectionErr
			}
			request.PlanProjection = &projection
		} else if request.Operation == store.DeliveryProjectInbox && request.WorkflowQuestions == nil {
			questions, questionsErr := g.Store.OpenWorkflowQuestions(operationCtx, request.Repository, 0)
			if questionsErr != nil {
				return store.DeliveryResult{}, questionsErr
			}
			request.WorkflowQuestions = make([]plan.WorkflowQuestion, 0, len(questions))
			for _, question := range questions {
				request.WorkflowQuestions = append(request.WorkflowQuestions, plan.WorkflowQuestion{
					ID: question.ID, Prompt: question.Prompt, Repository: question.Repository,
					PlanNumber: question.RootNumber, TicketNumber: question.TicketNumber,
					PullRequest: question.PullRequest, Commit: question.Commit, Finding: question.Kind, Diagnostics: question.Diagnostics,
					Evidence: question.Evidence,
				})
			}
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
		return errors.Join(err, leaseErr)
	}
	if err != nil {
		if isCredentialRejection(err) {
			return g.pauseForCredential(ctx, outbox, err)
		}
		if errors.Is(err, store.ErrDeliveryRejected) {
			return g.reject(ctx, outbox, err)
		}
		if errors.Is(err, store.ErrDeliveryUncertain) {
			if finishErr := g.Store.MarkDeliveryOutboxUncertain(ctx, outbox.IdempotencyKey, outbox.ClaimToken, err.Error(), g.now()); finishErr != nil {
				return finishErr
			}
			return err
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
		if _, err := g.Submit(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: repository}); err != nil {
			return err
		}
	}
	return nil
}

func (g Gateway) reconcileOnly(ctx context.Context, outbox store.DeliveryOutbox) error {
	observation, err := g.observe(ctx, outbox.Request)
	if err != nil {
		if isCredentialRejection(err) {
			return g.pauseForCredential(ctx, outbox, err)
		}
		if finishErr := g.Store.MarkDeliveryOutboxUncertain(ctx, outbox.IdempotencyKey, outbox.ClaimToken, err.Error(), g.now()); finishErr != nil {
			return finishErr
		}
		return err
	}
	if observation.Applied {
		return g.succeed(ctx, outbox, resultFrom(observation))
	}
	return g.reject(ctx, outbox, fmt.Errorf("%w: uncertain delivery was not observed", store.ErrDeliveryRejected))
}

func (g Gateway) controlPlaneDispatchContext(ctx context.Context, outbox store.DeliveryOutbox, dispatcherToken string) (context.Context, func() error, error) {
	if outbox.Request.RunID != "" {
		return ctx, func() error { return nil }, nil
	}
	if err := g.Store.RenewGatewayDispatcher(ctx, dispatcherToken, g.now()); err != nil {
		return nil, nil, err
	}
	operationCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan struct{})
	var renewalErr error
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
				if err := g.Store.RenewGatewayDispatcher(context.Background(), dispatcherToken, g.now()); err != nil {
					renewalErr = err
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
		return renewalErr
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

func (g Gateway) pauseForCredential(ctx context.Context, outbox store.DeliveryOutbox, cause error) error {
	reason := "Gateway Credential was rejected; replace and verify it to resume writes"
	if err := g.Store.PauseGatewayWrites(ctx, reason, g.now()); err != nil {
		return fmt.Errorf("%v; persist Gateway pause: %w", cause, err)
	}
	if err := g.QueueGatewayCredentialInboxProjections(ctx); err != nil {
		return fmt.Errorf("%v; queue Workflow Inbox recovery request: %w", cause, err)
	}
	_ = g.Store.FinishDeliveryOutbox(ctx, outbox.IdempotencyKey, outbox.ClaimToken, store.OutboxPending, reason, g.now())
	return fmt.Errorf("%w: %v", ErrGatewayWritesPaused, cause)
}

func isCredentialRejection(err error) bool {
	if errors.Is(err, ErrGatewayCredentialRejected) {
		return true
	}
	var failure authenticationFailure
	return errors.As(err, &failure) && failure.AuthenticationFailure()
}
