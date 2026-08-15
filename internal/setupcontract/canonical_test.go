package setupcontract

import (
	"encoding/json"
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

func TestParsePlanRejectsKindSpecificEffectParameterDrift(t *testing.T) {
	tests := []struct {
		name   string
		effect Effect
	}{
		{"platform CLI missing checksum", Effect{ID: "cli", Kind: "platform_cli", Subject: `C:\Workflow\bin\workflow.exe`, Action: "install", Parameters: map[string]string{"version": "1.0.0"}}},
		{"Docker unknown parameter", Effect{ID: "docker", Kind: "docker_desktop", Subject: "current-host", Action: "install", Parameters: map[string]string{"version": "4.45.0", "installer_url": "https://example.test/docker.exe", "windows_amd64_sha256": strings.Repeat("a", 64), "surprise": "true"}}},
		{"PAT invalid input contract", Effect{ID: "pat", Kind: "github_pat", Subject: `C:\Workflow\state\credentials\github.pat`, Action: "persist", Parameters: map[string]string{"input": "argument", "owner": "owner"}}},
		{"platform record lacks contract digest", Effect{ID: "record", Kind: "platform_installation", Subject: `C:\Workflow`, Action: "record", Parameters: map[string]string{"version": "1.0.0", "release_manifest_digest": strings.Repeat("a", 64), "platform_setup_contract_json": `{}`, "workflow_cli_sha256": strings.Repeat("b", 64)}}},
		{"unknown effect kind", Effect{ID: "unknown", Kind: "surprise", Subject: "target", Action: "mutate", Parameters: map[string]string{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := Plan{SchemaVersion: 1, PlanID: "validate-effects", Kind: PlatformBootstrap, Target: Target{WorkflowHome: `C:\Workflow`}, Preconditions: []Precondition{{ID: "release", Kind: "platform_release", Subject: "platform-v1.0.0", Expected: strings.Repeat("a", 64)}}, Effects: []Effect{test.effect}, ExpectedResults: []ExpectedResult{{ID: "ready", Kind: "platform_readiness", Subject: `C:\Workflow`, Expected: "ready"}}}
			raw, _ := json.Marshal(plan)
			if _, _, _, err := ParsePlan(raw); err == nil {
				t.Fatalf("accepted effect parameter drift: %#v", test.effect)
			}
		})
	}
}

func TestParsePlanRejectsUnknownSemanticCombinations(t *testing.T) {
	base := Plan{SchemaVersion: 1, PlanID: "semantic-registry", Kind: PlatformBootstrap, Target: Target{WorkflowHome: `C:\Workflow`}, Preconditions: []Precondition{{ID: "host", Kind: "host_identity", Subject: "current-user", Expected: "user"}}, Effects: []Effect{{ID: "file", Kind: "install_file", Subject: "workflow.exe", Action: "install", Parameters: map[string]string{"sha256": strings.Repeat("a", 64)}}}, ExpectedResults: []ExpectedResult{{ID: "file-ready", Kind: "file_digest", Subject: "workflow.exe", Expected: strings.Repeat("a", 64)}}}
	tests := map[string]func(*Plan){
		"unknown precondition":        func(p *Plan) { p.Preconditions[0].Kind = "surprise" },
		"wrong precondition for plan": func(p *Plan) { p.Preconditions[0].Kind = "git_head" },
		"unknown action":              func(p *Plan) { p.Effects[0].Action = "overwrite_anything" },
		"wrong effect for plan": func(p *Plan) {
			p.Effects[0] = Effect{ID: "repo", Kind: "create_repository", Subject: "owner/repo", Action: "create", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true"}}
		},
		"unknown expected result": func(p *Plan) { p.ExpectedResults[0].Kind = "surprise" },
		"wrong result for plan":   func(p *Plan) { p.ExpectedResults[0].Kind = "repository_admission" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			plan := base
			plan.Preconditions = append([]Precondition(nil), base.Preconditions...)
			plan.Effects = append([]Effect(nil), base.Effects...)
			plan.ExpectedResults = append([]ExpectedResult(nil), base.ExpectedResults...)
			mutate(&plan)
			raw, _ := json.Marshal(plan)
			if _, _, _, err := ParsePlan(raw); err == nil {
				t.Fatalf("accepted unsupported semantic combination: %#v", plan)
			}
		})
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
