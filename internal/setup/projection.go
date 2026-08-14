package setup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func Project(plan setupcontract.Plan, digest string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "Setup Plan %s\nKind: %s\nTarget: %s\nDigest (SHA-256): %s\n\nAuthorized effects:\n", plan.PlanID, plan.Kind, targetName(plan), digest)
	for _, effect := range plan.Effects {
		fmt.Fprintf(&output, "- %s: %s %s (%s)\n", effect.ID, effect.Action, effect.Subject, effect.Kind)
		keys := make([]string, 0, len(effect.Parameters))
		for key := range effect.Parameters {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := effect.Parameters[key]
			if key == "input" || strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "secret") {
				value = "<redacted>"
			}
			if strings.HasSuffix(key, "files_json") {
				if rendered, ok := projectFiles(value); ok {
					fmt.Fprintf(&output, "  %s:\n%s", key, rendered)
					continue
				}
			}
			fmt.Fprintf(&output, "  %s: %s\n", key, value)
		}
	}
	output.WriteString("\nExpected results:\n")
	for _, result := range plan.ExpectedResults {
		fmt.Fprintf(&output, "- %s: %s = %s\n", result.ID, result.Subject, result.Expected)
	}
	return output.String()
}

func projectFiles(raw string) (string, bool) {
	var encoded map[string]string
	if json.Unmarshal([]byte(raw), &encoded) != nil || len(encoded) == 0 {
		return "", false
	}
	paths := make([]string, 0, len(encoded))
	for path := range encoded {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var output strings.Builder
	for _, path := range paths {
		data, err := base64.StdEncoding.DecodeString(encoded[path])
		if err != nil {
			return "", false
		}
		fmt.Fprintf(&output, "    --- %s\n", path)
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			fmt.Fprintf(&output, "    + %s\n", line)
		}
	}
	return output.String(), true
}
