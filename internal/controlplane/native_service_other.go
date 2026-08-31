//go:build !windows && !darwin

package controlplane

import "errors"

func NewNativeService(NativeServiceOptions) (NativeService, error) {
	return nil, errors.New("native Control Plane service is supported only on Windows x64 and macOS Apple Silicon")
}
