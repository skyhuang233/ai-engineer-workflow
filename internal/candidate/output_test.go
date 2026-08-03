package candidate

import "testing"

func TestValidateMatchesCandidateSchema(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		valid  bool
	}{
		{name: "complete", output: `{"summary":"implemented","commit":"abc","checks":[{"command":"go test ./...","outcome":"passed"}]}`, valid: true},
		{name: "additional property", output: `{"summary":"implemented","unexpected":"value"}`},
		{name: "missing summary", output: `{"checks":[]}`},
		{name: "missing verification evidence", output: `{"summary":"implemented"}`},
		{name: "empty verification evidence", output: `{"summary":"implemented","checks":[]}`},
		{name: "null commit", output: `{"summary":"implemented","commit":null}`},
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
