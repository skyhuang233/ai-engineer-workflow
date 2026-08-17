//go:build !windows

package setup

import "errors"

func workflowHomeOwnerIdentity(string) (string, error) {
	return "", errors.New("Platform Bootstrap host identity is supported on Windows only")
}

func setWorkflowHomeOwnerIdentity(string, string) error {
	return errors.New("Platform Bootstrap host identity is supported on Windows only")
}
