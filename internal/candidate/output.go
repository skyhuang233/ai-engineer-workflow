package candidate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Schema is the strict contract supplied to OpenAI structured output. Every
// property is required, with null representing an unavailable commit.
const Schema = `{
  "type": "object",
  "required": ["summary", "commit", "checks", "plan_amendment"],
  "properties": {
    "summary": {"type": "string", "minLength": 1},
    "commit": {"type": ["string", "null"]},
    "checks": {
      "type": "array",
      "minItems": 0,
      "items": {
        "type": "object",
        "required": ["command", "outcome"],
        "properties": {
          "command": {"type": "string", "minLength": 1},
          "outcome": {"type": "string", "enum": ["passed"]}
        },
        "additionalProperties": false
      }
    },
    "plan_amendment": {
      "type": ["object", "null"],
      "required": ["summary", "add_tickets", "remove_ticket_ids", "add_dependencies", "remove_dependencies"],
      "properties": {
        "summary": {"type": "string", "minLength": 1},
        "add_tickets": {"type": "array"},
        "remove_ticket_ids": {"type": "array"},
        "add_dependencies": {"type": "array"},
        "remove_dependencies": {"type": "array"}
      },
      "additionalProperties": false
    }
  },
  "additionalProperties": false
}`

// Validate also accepts legacy output that omitted commit.
func Validate(output []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(output))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return errors.New("structured result is not a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("structured result is not a single JSON object")
	}
	for name := range fields {
		switch name {
		case "summary", "commit", "checks", "plan_amendment":
		default:
			return errors.New("structured result contains an unsupported property")
		}
	}
	summary, ok := fields["summary"]
	if !ok {
		return errors.New("structured result requires a nonempty summary")
	}
	var summaryText string
	if err := json.Unmarshal(summary, &summaryText); err != nil || jsonNull(summary) || strings.TrimSpace(summaryText) == "" {
		return errors.New("structured result requires a nonempty summary")
	}
	if amendment, ok := fields["plan_amendment"]; ok && !jsonNull(amendment) {
		var proposal map[string]json.RawMessage
		if err := json.Unmarshal(amendment, &proposal); err != nil || proposal == nil || len(proposal) != 5 {
			return errors.New("plan amendment requires a nonempty summary")
		}
		summary, ok := proposal["summary"]
		if !ok || json.Unmarshal(summary, &summaryText) != nil || jsonNull(summary) || strings.TrimSpace(summaryText) == "" {
			return errors.New("plan amendment requires a nonempty summary")
		}
		for _, name := range []string{"add_tickets", "remove_ticket_ids", "add_dependencies", "remove_dependencies"} {
			var values []json.RawMessage
			if raw, ok := proposal[name]; !ok || json.Unmarshal(raw, &values) != nil || jsonNull(raw) {
				return errors.New("plan amendment requires complete structural changes")
			}
		}
		return nil
	}
	commit, present := fields["commit"]
	if present && !jsonNull(commit) {
		var commitText string
		if err := json.Unmarshal(commit, &commitText); err != nil {
			return errors.New("structured result commit must be a string or null")
		}
	}
	checks, ok := fields["checks"]
	if !ok {
		return errors.New("structured result requires verification evidence")
	}
	var records []json.RawMessage
	if err := json.Unmarshal(checks, &records); err != nil || jsonNull(checks) || len(records) == 0 {
		return errors.New("structured result requires verification evidence")
	}
	for _, record := range records {
		var check map[string]json.RawMessage
		if err := json.Unmarshal(record, &check); err != nil || check == nil || jsonNull(record) {
			return errors.New("structured result checks must be objects")
		}
		if len(check) != 2 || check["command"] == nil || check["outcome"] == nil {
			return errors.New("structured result checks must include only command and outcome")
		}
		var command, outcome string
		if err := json.Unmarshal(check["command"], &command); err != nil || jsonNull(check["command"]) || strings.TrimSpace(command) == "" {
			return errors.New("structured result checks require a nonempty command")
		}
		if err := json.Unmarshal(check["outcome"], &outcome); err != nil || jsonNull(check["outcome"]) || outcome != "passed" {
			return errors.New("structured result checks require a passed outcome")
		}
	}
	return nil
}

func jsonNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
