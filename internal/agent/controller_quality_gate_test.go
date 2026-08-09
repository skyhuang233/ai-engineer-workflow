package agent

import (
	"strings"
	"testing"
)

func TestParseDeliveryOutcomeEnforcesQualityGateActions(t *testing.T) {
	completed := "run:\n  status: completed\noutcome: checks-passed\n"
	for name, test := range map[string]struct {
		output    string
		wantGate  bool
		wantError string
	}{
		"no-op continues": {
			output: completed + "gate:\n  action: no-op\n",
		},
		"auto-fix requires a finding ID": {
			output:    completed + "gate:\n  action: auto-fix\n",
			wantError: "omitted its finding ID",
		},
		"ask-user pauses even if the tool claims completion": {
			output:   completed + "gate:\n  id: gate-1\n  action: ask-user\n  reason: choose a migration\n  allowed_answers[1]: proceed\n",
			wantGate: true,
		},
		"skip pauses": {
			output:   completed + "gate:\n  id: gate-2\n  action: skip\n  reason: skip lint\n",
			wantGate: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			outcome, err := parseDeliveryOutcome([]byte(test.output))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("parse error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Passed == test.wantGate || (outcome.Gate != nil) != test.wantGate {
				t.Fatalf("outcome = %#v, want gate=%v", outcome, test.wantGate)
			}
			if name == "skip pauses" && len(outcome.Gate.AllowedAnswers) != 0 {
				t.Fatalf("skip gate parser unexpectedly supplied answers: %#v", outcome.Gate)
			}
		})
	}
}
