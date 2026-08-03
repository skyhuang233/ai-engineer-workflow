package candidate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const Schema = `{
  "type": "object",
  "required": ["summary", "checks"],
  "properties": {
    "summary": {"type": "string", "minLength": 1},
    "commit": {"type": "string"},
    "checks": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["command", "outcome"],
        "properties": {
          "command": {"type": "string", "minLength": 1},
          "outcome": {"type": "string", "enum": ["passed"]}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`

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
		case "summary", "commit", "checks":
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
	if commit, ok := fields["commit"]; ok {
		var commitText string
		if err := json.Unmarshal(commit, &commitText); err != nil || jsonNull(commit) {
			return errors.New("structured result commit must be a string")
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
