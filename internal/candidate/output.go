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
  "required": ["summary", "tests"],
  "properties": {
    "summary": {"type": "string", "minLength": 1},
    "commit": {"type": "string"},
    "tests": {"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}}
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
		case "summary", "commit", "tests":
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
	tests, ok := fields["tests"]
	if !ok {
		return errors.New("structured result requires verification evidence")
	}
	var testNames []json.RawMessage
	if err := json.Unmarshal(tests, &testNames); err != nil || jsonNull(tests) || len(testNames) == 0 {
		return errors.New("structured result requires verification evidence")
	}
	for _, testName := range testNames {
		var name string
		if err := json.Unmarshal(testName, &name); err != nil || jsonNull(testName) || strings.TrimSpace(name) == "" {
			return errors.New("structured result tests must be nonempty strings")
		}
	}
	return nil
}

func jsonNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
