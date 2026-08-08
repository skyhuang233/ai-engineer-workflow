package candidate

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSchemaMeetsOpenAIStrictObjectRequirements(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(Schema), &schema); err != nil {
		t.Fatal(err)
	}
	assertStrictObjectSchema(t, schema, "$")
	root := schema.(map[string]any)
	commit := root["properties"].(map[string]any)["commit"].(map[string]any)
	types := commit["type"].([]any)
	if len(types) != 2 || types[0] != "string" || types[1] != "null" {
		t.Fatalf("commit schema type = %#v, want string-or-null", types)
	}
}

func assertStrictObjectSchema(t *testing.T, node any, path string) {
	t.Helper()
	switch value := node.(type) {
	case []any:
		for index, child := range value {
			assertStrictObjectSchema(t, child, fmt.Sprintf("%s[%d]", path, index))
		}
	case map[string]any:
		if value["type"] == "object" {
			properties, ok := value["properties"].(map[string]any)
			if !ok {
				t.Fatalf("object schema %s has no properties object", path)
			}
			requiredValues, ok := value["required"].([]any)
			if !ok {
				t.Fatalf("object schema %s has no required array", path)
			}
			required := make(map[string]bool, len(requiredValues))
			for _, name := range requiredValues {
				required[fmt.Sprint(name)] = true
			}
			for name := range properties {
				if !required[name] {
					t.Errorf("object schema %s property %q is not required", path, name)
				}
			}
			if additional, ok := value["additionalProperties"].(bool); !ok || additional {
				t.Errorf("object schema %s must set additionalProperties to false", path)
			}
		}
		for name, child := range value {
			assertStrictObjectSchema(t, child, path+"."+name)
		}
	}
}

func TestValidateCandidateOutput(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		valid  bool
	}{
		{name: "complete", output: `{"summary":"implemented","commit":"abc","checks":[{"command":"go test ./...","outcome":"passed"}]}`, valid: true},
		{name: "nullable commit", output: `{"summary":"implemented","commit":null,"checks":[{"command":"go test ./...","outcome":"passed"}]}`, valid: true},
		{name: "legacy missing commit", output: `{"summary":"implemented","checks":[{"command":"go test ./...","outcome":"passed"}]}`, valid: true},
		{name: "structured plan amendment", output: `{"summary":"dependency is obsolete","commit":null,"checks":[],"plan_amendment":{"summary":"dependency is obsolete","add_tickets":[],"remove_ticket_ids":[],"add_dependencies":[],"remove_dependencies":[{"blocked_ticket_id":2,"blocker_ticket_id":1}]}}`, valid: true},
		{name: "incomplete plan amendment", output: `{"summary":"dependency is obsolete","plan_amendment":{"summary":"dependency is obsolete","add_tickets":[]}}`},
		{name: "additional property", output: `{"summary":"implemented","unexpected":"value"}`},
		{name: "missing summary", output: `{"checks":[]}`},
		{name: "missing verification evidence", output: `{"summary":"implemented"}`},
		{name: "empty verification evidence", output: `{"summary":"implemented","checks":[]}`},
		{name: "test label", output: `{"summary":"implemented","tests":["not run"]}`},
		{name: "unknown check property", output: `{"summary":"implemented","checks":[{"command":"go test","outcome":"passed","detail":"ok"}]}`},
		{name: "missing check command", output: `{"summary":"implemented","checks":[{"outcome":"passed"}]}`},
		{name: "empty check command", output: `{"summary":"implemented","checks":[{"command":" ","outcome":"passed"}]}`},
		{name: "null check", output: `{"summary":"implemented","checks":[null]}`},
		{name: "failed check", output: `{"summary":"implemented","checks":[{"command":"go test","outcome":"failed"}]}`},
		{name: "not run check", output: `{"summary":"implemented","checks":[{"command":"go test","outcome":"not run"}]}`},
		{name: "trailing JSON", output: `{"summary":"implemented"} {}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate([]byte(tt.output))
			if (err == nil) != tt.valid {
				t.Fatalf("Validate(%s) error = %v, valid = %t", tt.output, err, tt.valid)
			}
		})
	}
}
