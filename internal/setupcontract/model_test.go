package setupcontract

import (
	"testing"
	"time"
)

func TestExecutionResultValidatesAppendOnlyAttemptIdentity(t *testing.T) {
	result := ExecutionResult{
		SchemaVersion: 1,
		PlanID:        "platform-001",
		PlanDigest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AttemptID:     "attempt-001",
		StartedAt:     time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC),
		FinishedAt:    time.Date(2026, 8, 14, 10, 1, 0, 0, time.UTC),
		Status:        ExecutionIncomplete,
		Effects:       []EffectResult{{EffectID: "install-cli", Status: EffectSatisfied, Evidence: "sha256:aaaa"}},
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	result.Effects = append(result.Effects, result.Effects[0])
	if err := result.Validate(); err == nil {
		t.Fatal("accepted duplicate effect result")
	}
}
