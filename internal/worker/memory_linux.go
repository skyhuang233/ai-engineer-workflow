//go:build linux

package worker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func hostMemoryUsage(context.Context) (float64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	values := map[string]float64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSuffix(scanner.Text(), ":"))
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		values[strings.TrimSuffix(fields[0], ":")] = value
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	total := values["MemTotal"]
	available, found := values["MemAvailable"]
	if total <= 0 || !found || available < 0 {
		return 0, fmt.Errorf("incomplete /proc/meminfo")
	}
	return (total - available) * 100 / total, nil
}
