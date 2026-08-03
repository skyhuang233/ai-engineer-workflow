package delivery

import (
	"context"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

type PlanProjector struct{ Gateway Gateway }

func (p PlanProjector) ProjectPlan(ctx context.Context, repository string, rootNumber int64, projection plan.Projection, label string) error {
	request := store.DeliveryRequest{Operation: store.DeliveryProjectPlan, Repository: repository, RootNumber: rootNumber, PlanProjection: &projection}
	outbox, err := p.Gateway.Submit(ctx, request)
	if err != nil {
		return err
	}
	if err := p.Gateway.Dispatch(ctx, outbox.IdempotencyKey); err != nil {
		return err
	}
	if label == "" {
		return nil
	}
	request.Operation, request.Label, request.IdempotencyKey = store.DeliveryAddIssueLabel, label, ""
	outbox, err = p.Gateway.Submit(ctx, request)
	if err != nil {
		return err
	}
	return p.Gateway.Dispatch(ctx, outbox.IdempotencyKey)
}
