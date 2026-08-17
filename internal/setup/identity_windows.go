//go:build windows

package setup

import (
	"errors"

	"golang.org/x/sys/windows"
)

func workflowHomeOwnerIdentity(path string) (string, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	if descriptor == nil {
		return "", errors.New("Workflow Home has no security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return "", err
	}
	if owner == nil {
		return "", errors.New("Workflow Home has no owner")
	}
	return owner.String(), nil
}

func setWorkflowHomeOwnerIdentity(path, identity string) error {
	owner, err := windows.StringToSid(identity)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil)
}
