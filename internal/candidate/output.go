package candidate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

var fullLowercaseCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Schema is the strict contract supplied to OpenAI structured output. Every
// property is required, with null representing an unavailable commit.
const Schema = `{
  "type": "object",
  "required": ["summary", "commit", "checks", "plan_amendment"],
  "properties": {
    "summary": {"type": "string", "minLength": 1},
    "commit": {"type": ["string", "null"], "pattern": "^[0-9a-f]{40}$", "minLength": 40, "maxLength": 40},
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
        "add_tickets": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["ID", "NodeID", "Number", "Title", "Body", "State", "Labels", "UpdatedAt", "Delivered", "Author", "AuthorType"],
            "properties": {
              "ID": {"type": "integer"},
              "NodeID": {"type": "string"},
              "Number": {"type": "integer"},
              "Title": {"type": "string", "minLength": 1},
              "Body": {"type": "string"},
              "State": {"type": "string"},
              "Labels": {"type": "array", "items": {"type": "string"}},
              "UpdatedAt": {"type": "string"},
              "Delivered": {"type": "boolean"},
              "Author": {"type": "string"},
              "AuthorType": {"type": "string"}
            },
            "additionalProperties": false
          }
        },
        "remove_ticket_ids": {"type": "array", "items": {"type": "integer"}},
        "add_dependencies": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["blocked_ticket_id", "blocker_ticket_id"],
            "properties": {
              "blocked_ticket_id": {"type": "integer"},
              "blocker_ticket_id": {"type": "integer"}
            },
            "additionalProperties": false
          }
        },
        "remove_dependencies": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["blocked_ticket_id", "blocker_ticket_id"],
            "properties": {
              "blocked_ticket_id": {"type": "integer"},
              "blocker_ticket_id": {"type": "integer"}
            },
            "additionalProperties": false
          }
        }
      },
      "additionalProperties": false
    }
  },
  "additionalProperties": false
}`

// Validate also accepts legacy output that omitted commit.
func Validate(output []byte) error {
	fields, err := decodeObject(output)
	if err != nil {
		return err
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

func ValidateStrict(output []byte) error {
	fields, err := decodeObject(output)
	if err != nil {
		return err
	}
	if err := requireProperties(fields, "summary", "commit", "checks", "plan_amendment"); err != nil {
		return err
	}
	if err := nonemptyString(fields["summary"], "structured result requires a nonempty summary"); err != nil {
		return err
	}
	if !jsonNull(fields["commit"]) {
		var commit string
		if err := json.Unmarshal(fields["commit"], &commit); err != nil || !fullLowercaseCommitSHA.MatchString(commit) {
			return errors.New("structured result commit must be a full lowercase 40-character Git SHA or null")
		}
	}
	if err := validateChecks(fields["checks"]); err != nil {
		return err
	}
	if jsonNull(fields["plan_amendment"]) {
		return nil
	}
	return validatePlanAmendment(fields["plan_amendment"])
}

func ExtractCodexCandidate(output []byte) ([]byte, error) {
	var structured []byte
	for _, line := range strings.Split(string(output), "\n") {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &event) != nil || event.Type != "item.completed" || event.Item.Type != "agent_message" {
			continue
		}
		candidate := []byte(strings.TrimSpace(event.Item.Text))
		if ValidateStrict(candidate) == nil {
			structured = append([]byte(nil), candidate...)
		}
	}
	if len(structured) == 0 {
		return nil, errors.New("Codex output did not contain a valid Candidate structured response")
	}
	return structured, nil
}

func decodeObject(output []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("structured result is not a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("structured result is not a single JSON object")
	}
	return fields, nil
}

func requireProperties(fields map[string]json.RawMessage, names ...string) error {
	if len(fields) != len(names) {
		return errors.New("structured result does not match the Candidate schema")
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return errors.New("structured result does not match the Candidate schema")
		}
	}
	return nil
}

func nonemptyString(raw json.RawMessage, message string) error {
	var value string
	if jsonNull(raw) || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return errors.New(message)
	}
	return nil
}

func validateChecks(raw json.RawMessage) error {
	var records []json.RawMessage
	if jsonNull(raw) || json.Unmarshal(raw, &records) != nil {
		return errors.New("structured result checks must be an array")
	}
	for _, record := range records {
		check, err := decodeObject(record)
		if err != nil || requireProperties(check, "command", "outcome") != nil {
			return errors.New("structured result checks must include only command and outcome")
		}
		if err := nonemptyString(check["command"], "structured result checks require a nonempty command"); err != nil {
			return err
		}
		var outcome string
		if jsonNull(check["outcome"]) || json.Unmarshal(check["outcome"], &outcome) != nil || outcome != "passed" {
			return errors.New("structured result checks require a passed outcome")
		}
	}
	return nil
}

func validatePlanAmendment(raw json.RawMessage) error {
	proposal, err := decodeObject(raw)
	if err != nil || requireProperties(proposal, "summary", "add_tickets", "remove_ticket_ids", "add_dependencies", "remove_dependencies") != nil {
		return errors.New("plan amendment requires complete structural changes")
	}
	if err := nonemptyString(proposal["summary"], "plan amendment requires a nonempty summary"); err != nil {
		return err
	}
	var tickets []json.RawMessage
	if jsonNull(proposal["add_tickets"]) || json.Unmarshal(proposal["add_tickets"], &tickets) != nil {
		return errors.New("plan amendment add_tickets must be an array")
	}
	for _, ticket := range tickets {
		if err := validatePlanIssue(ticket); err != nil {
			return err
		}
	}
	var ticketIDs []json.RawMessage
	if jsonNull(proposal["remove_ticket_ids"]) || json.Unmarshal(proposal["remove_ticket_ids"], &ticketIDs) != nil {
		return errors.New("plan amendment remove_ticket_ids must contain integers")
	}
	for _, ticketID := range ticketIDs {
		var value int64
		if jsonNull(ticketID) || json.Unmarshal(ticketID, &value) != nil {
			return errors.New("plan amendment remove_ticket_ids must contain integers")
		}
	}
	for _, name := range []string{"add_dependencies", "remove_dependencies"} {
		var dependencies []json.RawMessage
		if jsonNull(proposal[name]) || json.Unmarshal(proposal[name], &dependencies) != nil {
			return errors.New("plan amendment dependencies must be arrays")
		}
		for _, dependency := range dependencies {
			fields, err := decodeObject(dependency)
			if err != nil || requireProperties(fields, "blocked_ticket_id", "blocker_ticket_id") != nil {
				return errors.New("plan amendment dependencies must match the Candidate schema")
			}
			for _, field := range []string{"blocked_ticket_id", "blocker_ticket_id"} {
				var id int64
				if jsonNull(fields[field]) || json.Unmarshal(fields[field], &id) != nil {
					return errors.New("plan amendment dependency IDs must be integers")
				}
			}
		}
	}
	return nil
}

func validatePlanIssue(raw json.RawMessage) error {
	fields, err := decodeObject(raw)
	names := []string{"ID", "NodeID", "Number", "Title", "Body", "State", "Labels", "UpdatedAt", "Delivered", "Author", "AuthorType"}
	if err != nil || requireProperties(fields, names...) != nil {
		return errors.New("plan amendment tickets must match the Plan Issue contract")
	}
	for _, name := range []string{"ID", "Number"} {
		var value int64
		if jsonNull(fields[name]) || json.Unmarshal(fields[name], &value) != nil {
			return errors.New("plan amendment ticket IDs must be integers")
		}
	}
	for _, name := range []string{"NodeID", "Body", "State", "UpdatedAt", "Author", "AuthorType"} {
		var value string
		if jsonNull(fields[name]) || json.Unmarshal(fields[name], &value) != nil {
			return errors.New("plan amendment ticket text fields must be strings")
		}
	}
	if err := nonemptyString(fields["Title"], "plan amendment ticket requires a nonempty title"); err != nil {
		return err
	}
	var labels []json.RawMessage
	if jsonNull(fields["Labels"]) || json.Unmarshal(fields["Labels"], &labels) != nil {
		return errors.New("plan amendment ticket labels must be strings")
	}
	for _, label := range labels {
		var value string
		if jsonNull(label) || json.Unmarshal(label, &value) != nil {
			return errors.New("plan amendment ticket labels must be strings")
		}
	}
	var delivered bool
	if jsonNull(fields["Delivered"]) || json.Unmarshal(fields["Delivered"], &delivered) != nil {
		return errors.New("plan amendment ticket Delivered must be a boolean")
	}
	return nil
}

func jsonNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}
