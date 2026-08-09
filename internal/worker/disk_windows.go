//go:build windows

package worker

import (
	"os"

	"golang.org/x/sys/windows"
)

func hostDiskUsage(path string) (float64, error) {
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return 0, err
		}
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &available, &total, &free); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, os.ErrInvalid
	}
	return float64(total-free) * 100 / float64(total), nil
}
