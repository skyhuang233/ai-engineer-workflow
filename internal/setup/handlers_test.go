package setup

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func TestEveryCurrentSetupEffectHasOneCompleteHandler(t *testing.T) {
	want := []string{
		"control_plane",
		"create_repository",
		"docker_desktop",
		"github_label",
		"github_pat",
		"initial_baseline",
		"local_fast_forward",
		"platform_cli",
		"platform_installation",
		"publish_history",
		"repository_admission",
		"repository_contract_pr",
		"repository_features",
		"workflow_skill_bundle",
	}
	got := make([]string, 0, len(effectHandlers))
	for kind, handler := range effectHandlers {
		got = append(got, kind)
		if handler.contract.Kind != kind {
			t.Errorf("handler %q is linked to contract %q", kind, handler.contract.Kind)
		}
		if handler.readback == nil || handler.apply == nil || handler.afterSatisfied == nil || handler.finalize == nil {
			t.Errorf("handler %q is incomplete", kind)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setup effect handler registry drifted\n got: %v\nwant: %v", got, want)
	}
}

func TestHostAdapterFailsClosedForUnknownEffectKind(t *testing.T) {
	effect := setupcontract.Effect{ID: "unknown", Kind: "unknown", Subject: "subject", Action: "mutate", Parameters: map[string]string{}}
	adapter := HostAdapter{}
	if status, _, err := adapter.Readback(context.Background(), effect); err == nil || status != setupcontract.EffectFailed {
		t.Fatalf("Readback() = (%q, %v), want failed status and error", status, err)
	}
	if err := adapter.Apply(context.Background(), effect, nil); err == nil {
		t.Fatal("Apply() succeeded for an unknown effect kind")
	}
}
