package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

type fakeAdapter struct {
	states  map[string]setupcontract.EffectStatus
	applied []string
	fail    string
}

func (f *fakeAdapter) Readback(_ context.Context, e setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	state := f.states[e.ID]
	if state == "" {
		state = setupcontract.EffectRequired
	}
	return state, string(state), nil
}
func (f *fakeAdapter) Apply(_ context.Context, e setupcontract.Effect, _ *SecretInput) error {
	if e.ID == f.fail {
		return errors.New("injected failure")
	}
	f.applied = append(f.applied, e.ID)
	f.states[e.ID] = setupcontract.EffectSatisfied
	return nil
}

func TestEngineAppliesRequiredEffectsAndRetriesSatisfiedOnes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	_, _, digest, err := setupcontract.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}, fail: "second"}
	engine := Engine{Adapter: adapter, SecretInput: &SecretInput{Reader: bytes.NewBufferString("secret\n")}}
	first, err := engine.Apply(context.Background(), raw, digest)
	if err == nil || first.Status != setupcontract.ExecutionIncomplete {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	adapter.fail = ""
	second, err := engine.Apply(context.Background(), raw, digest)
	if err != nil || second.Status != setupcontract.ExecutionSucceeded {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if len(adapter.applied) != 2 || adapter.applied[0] != "first" || adapter.applied[1] != "second" {
		t.Fatalf("applied=%v", adapter.applied)
	}
}

func TestEngineRejectsDigestBeforeMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "WorkflowHome")
	plan := testPlan(home)
	raw, _ := json.Marshal(plan)
	adapter := &fakeAdapter{states: map[string]setupcontract.EffectStatus{}}
	_, err := (&Engine{Adapter: adapter}).Apply(context.Background(), raw, repeat("0", 64))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err=%v", err)
	}
	if len(adapter.applied) != 0 {
		t.Fatal("digest mismatch mutated host")
	}
}

func testPlan(home string) setupcontract.Plan {
	return setupcontract.Plan{SchemaVersion: 1, PlanID: "plan-test", Kind: setupcontract.PlatformBootstrap, Target: setupcontract.Target{WorkflowHome: home}, Preconditions: []setupcontract.Precondition{{ID: "release", Kind: "release", Subject: "v1", Expected: "ok"}}, Effects: []setupcontract.Effect{{ID: "first", Kind: "test", Subject: "one", Action: "create", Parameters: map[string]string{}}, {ID: "second", Kind: "test", Subject: "two", Action: "create", Parameters: map[string]string{}}}, ExpectedResults: []setupcontract.ExpectedResult{{ID: "ready", Kind: "platform", Subject: home, Expected: "ready"}}}
}
func repeat(value string, count int) string {
	var b bytes.Buffer
	for i := 0; i < count; i++ {
		b.WriteString(value)
	}
	return b.String()
}
