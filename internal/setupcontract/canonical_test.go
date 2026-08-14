package setupcontract

import (
	"strings"
	"testing"
)

func TestCanonicalizeNormalizesObjectOrderAndWhitespace(t *testing.T) {
	left := []byte("{\n  \"z\": [true, null, 7], \"a\": \"line\\n雪\", \"path\": \"C:\\\\Users\\\\Ada\\\\AgentWorkflow\"\n}")
	right := []byte("{\"path\":\"C:\\\\Users\\\\Ada\\\\AgentWorkflow\",\"a\":\"line\\n雪\",\"z\":[true,null,7]}")

	canonical, digest, err := Canonicalize(left)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := `{"a":"line\n雪","path":"C:\\Users\\Ada\\AgentWorkflow","z":[true,null,7]}`
	if string(canonical) != wantCanonical {
		t.Fatalf("canonical JSON = %s, want %s", canonical, wantCanonical)
	}
	if digest != "be48f4a3b27666d105bb2d9d84a569f6688cd7dfc10b93d106ba836bb1a0cefb" {
		t.Fatalf("digest = %s", digest)
	}
	canonicalAgain, digestAgain, err := Canonicalize(right)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalAgain) != string(canonical) || digestAgain != digest {
		t.Fatalf("semantic JSON changed authority: %s/%s versus %s/%s", canonical, digest, canonicalAgain, digestAgain)
	}
}

func TestCanonicalizeRejectsValuesOutsideRestrictedJSON(t *testing.T) {
	tests := map[string][]byte{
		"fraction":        []byte(`{"value":1.5}`),
		"exponent":        []byte(`{"value":1e3}`),
		"trailing value":  []byte(`{} {}`),
		"invalid UTF-8":   append([]byte(`{"value":"`), 0xff, '"', '}'),
		"unpaired escape": []byte(`{"value":"\ud800"}`),
		"duplicate field": []byte(`{"value":1,"value":2}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Canonicalize(raw); err == nil {
				t.Fatal("accepted value outside the restricted canonical JSON scheme")
			}
		})
	}
}

func TestParseValidateAndRoundTripPlan(t *testing.T) {
	raw := validPlatformPlanJSON()
	plan, canonical, digest, err := ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != PlatformBootstrap || plan.Target.WorkflowHome != `C:\Users\Ada\AgentWorkflow` {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	loaded, canonicalAgain, digestAgain, err := ParsePlan(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PlanID != plan.PlanID || string(canonicalAgain) != string(canonical) || digestAgain != digest {
		t.Fatal("validated plan did not survive serialize/load/hash")
	}
}

func TestPlanValidationFailsClosed(t *testing.T) {
	tests := map[string]func(string) string{
		"unknown field": func(s string) string { return strings.Replace(s, `"plan_id":`, `"surprise":true,"plan_id":`, 1) },
		"omitted field": func(s string) string { return strings.Replace(s, `"plan_id": "platform-001",`, "", 1) },
		"unknown kind":  func(s string) string { return strings.Replace(s, `"platform_bootstrap"`, `"mystery"`, 1) },
		"relative home": func(s string) string { return strings.Replace(s, `C:\\Users\\Ada\\AgentWorkflow`, `relative\\home`, 1) },
		"duplicate effect": func(s string) string {
			return strings.Replace(s, `  ] ,"expected_results"`, `    ,{"id":"install-cli","kind":"install_file","subject":"workflow.exe","action":"install","parameters":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
  ] ,"expected_results"`, 1)
		},
		"secret field": func(s string) string { return strings.Replace(s, `"sha256":`, `"github_token":`, 1) },
		"secret value": func(s string) string {
			return strings.Replace(s, strings.Repeat("a", 64), `ghp_abcdefghijklmnopqrstuvwxyz0123456789`, 1)
		},
	}
	base := string(validPlatformPlanJSON())
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := ParsePlan([]byte(mutate(base))); err == nil {
				t.Fatal("accepted invalid Setup Plan")
			}
		})
	}
}

func validPlatformPlanJSON() []byte {
	return []byte(`{
  "schema_version": 1,
  "plan_id": "platform-001",
  "kind": "platform_bootstrap",
  "target": {
    "workflow_home": "C:\\Users\\Ada\\AgentWorkflow",
    "repository_path": "",
    "github_repository": ""
  },
  "preconditions": [
    {"id":"windows-user","kind":"host_identity","subject":"current-user","expected":"S-1-5-21"}
  ],
  "effects": [
    {"id":"install-cli","kind":"install_file","subject":"workflow.exe","action":"install","parameters":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
  ] ,"expected_results": [
    {"id":"cli-ready","kind":"file_digest","subject":"workflow.exe","expected":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
  ]
}`)
}
