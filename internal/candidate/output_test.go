package candidate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/plan"
)

func TestSchemaMeetsOpenAIStrictObjectRequirements(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(Schema), &schema); err != nil {
		t.Fatal(err)
	}
	if err := validateStrictSchema(schema, "$"); err != nil {
		t.Fatal(err)
	}
	root := schema.(map[string]any)
	commit := root["properties"].(map[string]any)["commit"].(map[string]any)
	types := commit["type"].([]any)
	if len(types) != 2 || types[0] != "string" || types[1] != "null" {
		t.Fatalf("commit schema type = %#v, want string-or-null", types)
	}
}

func validateStrictSchema(node any, path string) error {
	switch value := node.(type) {
	case []any:
		for index, child := range value {
			if err := validateStrictSchema(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case map[string]any:
		if schemaHasType(value["type"], "array") {
			items, ok := value["items"].(map[string]any)
			if !ok || len(items) == 0 || !hasSchemaType(items["type"]) {
				return fmt.Errorf("array schema %s has no typed items schema", path)
			}
		}
		if schemaHasType(value["type"], "object") {
			properties, ok := value["properties"].(map[string]any)
			if !ok {
				return fmt.Errorf("object schema %s has no properties object", path)
			}
			requiredValues, ok := value["required"].([]any)
			if !ok {
				return fmt.Errorf("object schema %s has no required array", path)
			}
			required := make(map[string]bool, len(requiredValues))
			for _, name := range requiredValues {
				required[fmt.Sprint(name)] = true
			}
			for name := range properties {
				if !required[name] {
					return fmt.Errorf("object schema %s property %q is not required", path, name)
				}
			}
			if additional, ok := value["additionalProperties"].(bool); !ok || additional {
				return fmt.Errorf("object schema %s must set additionalProperties to false", path)
			}
		}
		for name, child := range value {
			if err := validateStrictSchema(child, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaHasType(value any, want string) bool {
	switch types := value.(type) {
	case string:
		return types == want
	case []any:
		for _, value := range types {
			if value == want {
				return true
			}
		}
	}
	return false
}

func hasSchemaType(value any) bool {
	allowed := map[string]bool{"array": true, "boolean": true, "integer": true, "null": true, "number": true, "object": true, "string": true}
	switch types := value.(type) {
	case string:
		return allowed[types]
	case []any:
		if len(types) == 0 {
			return false
		}
		for _, value := range types {
			typeName, ok := value.(string)
			if !ok || !allowed[typeName] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func TestStrictSchemaRejectsIncompleteArrayItems(t *testing.T) {
	for _, items := range []string{"", `null`, `{}`, `{"properties": {}}`, `{"type": null}`, `{"type": []}`, `{"type": "unknown"}`} {
		raw := `{"type":"array"`
		if items != "" {
			raw += `,"items":` + items
		}
		raw += `}`
		var schema any
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatal(err)
		}
		if err := validateStrictSchema(schema, "$"); err == nil {
			t.Errorf("validateStrictSchema(%s) accepted incomplete array items", raw)
		}
	}
	var nullableArray any
	if err := json.Unmarshal([]byte(`{"type":["array","null"],"items":{}}`), &nullableArray); err != nil {
		t.Fatal(err)
	}
	if err := validateStrictSchema(nullableArray, "$"); err == nil {
		t.Error("validateStrictSchema accepted nullable array with untyped items")
	}
}

func TestPlanAmendmentTicketSchemaMatchesPlanIssueJSONContract(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(Schema), &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	amendment := properties["plan_amendment"].(map[string]any)
	ticket := amendment["properties"].(map[string]any)["add_tickets"].(map[string]any)["items"].(map[string]any)

	var issue map[string]any
	encoded, err := json.Marshal(plan.Issue{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &issue); err != nil {
		t.Fatal(err)
	}
	issueFields := make([]string, 0, len(issue))
	for name := range issue {
		issueFields = append(issueFields, name)
	}
	sort.Strings(issueFields)
	schemaFields := make([]string, 0, len(ticket["properties"].(map[string]any)))
	for name := range ticket["properties"].(map[string]any) {
		schemaFields = append(schemaFields, name)
	}
	sort.Strings(schemaFields)
	if !reflect.DeepEqual(schemaFields, issueFields) {
		t.Fatalf("add_tickets item properties = %v, want plan.Issue fields %v", schemaFields, issueFields)
	}
	required := make([]string, 0, len(ticket["required"].([]any)))
	for _, name := range ticket["required"].([]any) {
		required = append(required, name.(string))
	}
	sort.Strings(required)
	if !reflect.DeepEqual(required, issueFields) {
		t.Fatalf("add_tickets item required = %v, want plan.Issue fields %v", required, issueFields)
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

func TestValidateStrictCandidateOutput(t *testing.T) {
	valid := `{"summary":"implemented","commit":null,"checks":[],"plan_amendment":null}`
	validAmendment := `{"summary":"replan","commit":null,"checks":[],"plan_amendment":{"summary":"replan","add_tickets":[{"ID":1,"NodeID":"node","Number":2,"Title":"ticket","Body":"body","State":"open","Labels":["workflow:ticket"],"UpdatedAt":"now","Delivered":false,"Author":"agent","AuthorType":"Bot"}],"remove_ticket_ids":[3],"add_dependencies":[{"blocked_ticket_id":2,"blocker_ticket_id":1}],"remove_dependencies":[]}}`
	for _, tt := range []struct {
		name   string
		output string
		valid  bool
	}{
		{name: "complete", output: valid, valid: true},
		{name: "complete amendment", output: validAmendment, valid: true},
		{name: "missing commit", output: `{"summary":"implemented","checks":[],"plan_amendment":null}`},
		{name: "missing plan amendment", output: `{"summary":"implemented","commit":null,"checks":[]}`},
		{name: "missing ticket field", output: `{"summary":"replan","commit":null,"checks":[],"plan_amendment":{"summary":"replan","add_tickets":[{"ID":1}],"remove_ticket_ids":[],"add_dependencies":[],"remove_dependencies":[]}}`},
		{name: "untyped ticket labels", output: `{"summary":"replan","commit":null,"checks":[],"plan_amendment":{"summary":"replan","add_tickets":[{"ID":1,"NodeID":"node","Number":2,"Title":"ticket","Body":"body","State":"open","Labels":[1],"UpdatedAt":"now","Delivered":false,"Author":"agent","AuthorType":"Bot"}],"remove_ticket_ids":[],"add_dependencies":[],"remove_dependencies":[]}}`},
		{name: "null ticket label", output: `{"summary":"replan","commit":null,"checks":[],"plan_amendment":{"summary":"replan","add_tickets":[{"ID":1,"NodeID":"node","Number":2,"Title":"ticket","Body":"body","State":"open","Labels":[null],"UpdatedAt":"now","Delivered":false,"Author":"agent","AuthorType":"Bot"}],"remove_ticket_ids":[],"add_dependencies":[],"remove_dependencies":[]}}`},
		{name: "null removed ticket ID", output: `{"summary":"replan","commit":null,"checks":[],"plan_amendment":{"summary":"replan","add_tickets":[],"remove_ticket_ids":[null],"add_dependencies":[],"remove_dependencies":[]}}`},
		{name: "untyped dependency", output: `{"summary":"replan","commit":null,"checks":[],"plan_amendment":{"summary":"replan","add_tickets":[],"remove_ticket_ids":[],"add_dependencies":[{"blocked_ticket_id":"2","blocker_ticket_id":1}],"remove_dependencies":[]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStrict([]byte(tt.output))
			if (err == nil) != tt.valid {
				t.Fatalf("ValidateStrict(%s) error = %v, valid = %t", tt.output, err, tt.valid)
			}
		})
	}
}

func TestExtractCodexCandidateSelectsFinalValidAgentMessage(t *testing.T) {
	first := `{"summary":"first","commit":null,"checks":[],"plan_amendment":null}`
	final := `{"summary":"final","commit":null,"checks":[],"plan_amendment":null}`
	output := appendCodexAgentMessages(first, final, `{}`)
	structured, err := ExtractCodexCandidate(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(structured) != final {
		t.Fatalf("ExtractCodexCandidate() = %s, want final valid message %s", structured, final)
	}
}

func appendCodexAgentMessages(messages ...string) []byte {
	var output strings.Builder
	for _, message := range messages {
		event, _ := json.Marshal(map[string]any{
			"type": "item.completed",
			"item": map[string]string{"type": "agent_message", "text": message},
		})
		output.Write(event)
		output.WriteByte('\n')
	}
	return []byte(output.String())
}
