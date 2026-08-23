package isolation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

const (
	defaultTimeout            = 30 * time.Second
	defaultTransitionAttempts = 5
	defaultRetryDelay         = 10 * time.Millisecond
)

type Store interface {
	FenceWorkerIsolation(context.Context, []store.TicketClaim) ([]store.TicketClaim, error)
	AcknowledgeWorkerIsolation(context.Context, []store.TicketClaim) ([]store.WorkerIsolationProof, error)
}

type ContainerIsolator interface {
	IsolateContainer(context.Context, string) error
}

func RetryWorkerTransition(ctx context.Context, database Store, isolator ContainerIsolator, transition func([]store.WorkerIsolationProof) error) error {
	if transition == nil {
		return errors.New("Worker transition is required")
	}
	var isolated []store.WorkerIsolationProof
	for attempt := 0; attempt < defaultTransitionAttempts; attempt++ {
		err := transition(isolated)
		var required *store.WorkerIsolationRequired
		if !errors.As(err, &required) {
			return err
		}
		acknowledged, isolationErr := IsolateWorkers(ctx, database, isolator, required.Targets)
		if isolationErr != nil {
			if !errors.Is(isolationErr, store.ErrFencingConflict) {
				return errors.Join(err, isolationErr)
			}
		} else {
			isolated = append(isolated, acknowledged...)
		}
		if attempt+1 == defaultTransitionAttempts {
			retryErr := errors.New("Worker transition isolation retry limit reached")
			if isolationErr != nil {
				retryErr = fmt.Errorf("Worker transition isolation retry limit reached: %w", isolationErr)
			}
			return errors.Join(err, retryErr)
		}
		timer := time.NewTimer(defaultRetryDelay << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, isolationErr, ctx.Err())
		case <-timer.C:
		}
	}
	return errors.New("Worker transition isolation retry limit reached")
}

func IsolateWorkers(ctx context.Context, database Store, isolator ContainerIsolator, targets []store.TicketClaim) ([]store.WorkerIsolationProof, error) {
	if database == nil || isolator == nil {
		return nil, errors.New("Worker isolation dependencies are incomplete")
	}
	fenced, err := database.FenceWorkerIsolation(ctx, targets)
	if err != nil {
		return nil, fmt.Errorf("fence Worker isolation: %w", err)
	}
	persistenceCtx := context.WithoutCancel(ctx)
	for _, target := range fenced {
		isolationCtx, cancel := context.WithTimeout(persistenceCtx, defaultTimeout)
		isolateErr := isolator.IsolateContainer(isolationCtx, target.RunID)
		cancel()
		if isolateErr != nil {
			return nil, fmt.Errorf("isolate Worker %s: %w", target.RunID, isolateErr)
		}
	}
	acknowledgeCtx, cancel := context.WithTimeout(persistenceCtx, defaultTimeout)
	acknowledged, err := database.AcknowledgeWorkerIsolation(acknowledgeCtx, fenced)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("acknowledge Worker isolation: %w", err)
	}
	return acknowledged, nil
}
