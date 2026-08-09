//go:build windows

package worker

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func hostMemoryUsage(ctx context.Context) (float64, error) {
	output, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", "$os=Get-CimInstance Win32_OperatingSystem; \"$($os.FreePhysicalMemory) $($os.TotalVisibleMemorySize)\"").Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return 0, fmt.Errorf("unexpected host memory output %q", output)
	}
	free, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	total, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || total <= 0 {
		return 0, fmt.Errorf("invalid host memory total %q", fields[1])
	}
	return (total - free) * 100 / total, nil
}
