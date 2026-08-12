package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

func TestDeliverySourcePreflightClassifiesUnavailableDigestHelperAsInfrastructure(t *testing.T) {
	if !strings.Contains(deliverySourceContainerPreflight, `126|127) infrastructure_failure`) {
		t.Fatal("Delivery Source preflight does not classify unavailable digest helper as infrastructure")
	}
	if strings.Contains(deliverySourceContainerPreflight, `actual=$(delivery-source-digest /source-seed) || integrity_failure`) {
		t.Fatal("Delivery Source preflight collapses digest helper availability into integrity failure")
	}
}

type recordingDeliveryRuntime struct {
	specs []worker.Spec
}

func (r *recordingDeliveryRuntime) Run(_ context.Context, spec worker.Spec) (worker.Result, error) {
	r.specs = append(r.specs, spec)
	return worker.Result{}, nil
}

func TestDeliveryControllerFailsClosedWithoutWorkflowIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for name, session := range map[string]store.TicketSession{
		"missing delivery cycle": {AcceptedCandidateRunID: "candidate-run"},
		"missing revision round": {SessionID: "ticket-session"},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := &recordingDeliveryRuntime{}
			controller := Controller{Store: db, Runtime: runtime, GatewayURL: "http://gateway.test"}

			err := controller.runDeliveryController(ctx, store.TicketClaim{}, session, workspace{}, store.CandidatePublication{}, "deliver")
			if err == nil || !strings.Contains(err.Error(), "Delivery Cycle or Revision Round is incomplete") {
				t.Fatalf("missing workflow identity error = %v", err)
			}
			if len(runtime.specs) != 0 {
				t.Fatalf("Delivery Controller launched without workflow identity: %#v", runtime.specs)
			}
		})
	}
}
