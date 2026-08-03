package candidate

import "testing"

func TestValidateMatchesCandidateSchema(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		valid  bool
	}{
		{name: "complete", output: `{"summary":"implemented","commit":"abc","tests":["go test"]}`, valid: true},
		{name: "additional property", output: `{"summary":"implemented","unexpected":"value"}`},
		{name: "missing summary", output: `{"tests":[]}`},
		{name: "missing verification evidence", output: `{"summary":"implemented"}`},
		{name: "empty verification evidence", output: `{"summary":"implemented","tests":[]}`},
		{name: "null commit", output: `{"summary":"implemented","commit":null}`},
		{name: "non-string test", output: `{"summary":"implemented","tests":[1]}`},
		{name: "null test", output: `{"summary":"implemented","tests":[null]}`},
		{name: "empty test", output: `{"summary":"implemented","tests":[" "]}`},
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
