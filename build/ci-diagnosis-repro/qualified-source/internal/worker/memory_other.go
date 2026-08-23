//go:build !windows && !linux

package worker

import (
	"context"
	"fmt"
)

func hostMemoryUsage(context.Context) (float64, error) {
	return 0, fmt.Errorf("host memory inspection is unsupported on this platform")
}
