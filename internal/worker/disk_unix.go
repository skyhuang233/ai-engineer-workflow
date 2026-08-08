//go:build !windows

package worker

import (
	"os"
	"syscall"
)

func hostDiskUsage(path string) (float64, error) {
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return 0, err
		}
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, os.ErrInvalid
	}
	return float64(stat.Blocks-stat.Bfree) * 100 / float64(stat.Blocks), nil
}
