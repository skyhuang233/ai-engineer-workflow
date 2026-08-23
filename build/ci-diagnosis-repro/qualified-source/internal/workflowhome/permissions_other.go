//go:build !windows

package workflowhome

import "errors"

func SecureCredentialPath(string, bool) error {
	return errors.New("Agent Workflow setup supports Windows only")
}
