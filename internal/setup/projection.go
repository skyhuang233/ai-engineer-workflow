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
	fmt.Fprintf(&output, "Setup Plan %s\nKind: %s\nTarget: %s\nDigest (SHA-256): %s\n\nPreconditions:\n", plan.PlanID, plan.Kind, targetName(plan), digest)
	for _, precondition := range plan.Preconditions {
		fmt.Fprintf(&output, "- %s: %s (%s) = %s\n", precondition.ID, precondition.Subject, precondition.Kind, precondition.Expected)
	}
	output.WriteString("\nAuthorized effects:\n")
	for _, effect := range plan.Effects {
		fmt.Fprintf(&output, "- %s: %s %s (%s)\n", effect.ID, effect.Action, effect.Subject, effect.Kind)
		keys := make([]string, 0, len(effect.Parameters))
		for key := range effect.Parameters {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == "before_files_json" {
				continue
			}
			value := effect.Parameters[key]
			if key == "input" || strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "secret") {
				value = "<redacted>"
			}
			if key == "files_json" {
				if rendered, ok := projectFileDiff(effect.Parameters["before_files_json"], value); ok {
					fmt.Fprintf(&output, "  %s:\n%s", key, rendered)
					continue
				}
			}
			fmt.Fprintf(&output, "  %s: %s\n", key, value)
		}
	}
	output.WriteString("\nExpected results:\n")
	for _, result := range plan.ExpectedResults {
		fmt.Fprintf(&output, "- %s: %s (%s) = %s\n", result.ID, result.Subject, result.Kind, result.Expected)
	}
	return output.String()
}

func projectFileDiff(beforeRaw, afterRaw string) (string, bool) {
	before := map[string]string{}
	after := map[string]string{}
	if beforeRaw != "" && json.Unmarshal([]byte(beforeRaw), &before) != nil {
		return "", false
	}
	if json.Unmarshal([]byte(afterRaw), &after) != nil || len(before)+len(after) == 0 {
		return "", false
	}
	pathSet := map[string]bool{}
	for path := range before {
		pathSet[path] = true
	}
	for path := range after {
		pathSet[path] = true
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var output strings.Builder
	for _, path := range paths {
		beforeText, beforeExists, ok := decodeProjectedFile(before, path)
		if !ok {
			return "", false
		}
		afterText, afterExists, ok := decodeProjectedFile(after, path)
		if !ok {
			return "", false
		}
		if beforeExists && afterExists && beforeText == afterText {
			continue
		}
		beforeName, afterName := path, path
		if !beforeExists {
			beforeName = "/dev/null"
		}
		if !afterExists {
			afterName = "/dev/null"
		}
		fmt.Fprintf(&output, "    --- %s\n    +++ %s\n", beforeName, afterName)
		for _, line := range projectedLines(beforeText, beforeExists) {
			fmt.Fprintf(&output, "    - %s\n", line)
		}
		for _, line := range projectedLines(afterText, afterExists) {
			fmt.Fprintf(&output, "    + %s\n", line)
		}
	}
	return output.String(), true
}

func decodeProjectedFile(encoded map[string]string, path string) (string, bool, bool) {
	value, exists := encoded[path]
	if !exists {
		return "", false, true
	}
	data, err := base64.StdEncoding.DecodeString(value)
	return string(data), true, err == nil
}

func projectedLines(value string, exists bool) []string {
	if !exists || value == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(value, "\n"), "\n")
}
