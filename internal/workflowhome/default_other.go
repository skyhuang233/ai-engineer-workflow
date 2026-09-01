//go:build !windows && !darwin

package workflowhome

import "errors"

func defaultWorkflowHomeRoot() (string, error) {
	return "", errors.New("Workflow Home is supported only on Windows x64 and macOS Apple Silicon")
}
