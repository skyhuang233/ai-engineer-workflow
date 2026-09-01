package controlplane

import (
	"context"
	"encoding/xml"
	"errors"
	"path/filepath"
	"strings"
)

const NativeServiceName = `\AgentWorkflow\ControlPlane`

// NativeService owns the current user's Control Plane lifetime. Setup only
// queries its fixed identity and registers it when absent; it never modifies
// an existing service definition or starts it manually.
type NativeService interface {
	Present(context.Context) (bool, error)
	Register(context.Context) error
}

type NativeServiceOptions struct {
	Executable   string
	WorkflowHome string
}

func (o NativeServiceOptions) validate() error {
	if strings.TrimSpace(o.Executable) == "" || !filepath.IsAbs(o.Executable) {
		return errors.New("native Control Plane service requires an absolute Host Executable")
	}
	if strings.TrimSpace(o.WorkflowHome) == "" || !filepath.IsAbs(o.WorkflowHome) {
		return errors.New("native Control Plane service requires an absolute Workflow Home")
	}
	return nil
}

func xmlEscape(value string) string {
	var destination strings.Builder
	_ = xml.EscapeText(&destination, []byte(value))
	return destination.String()
}
