// Package hostinfo is hostd's Host driver: what the host has besides its
// pool, read from /proc.
package hostinfo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Host reads the host's memory and CPUs.
type Host struct {
	// Meminfo is /proc/meminfo, unless set otherwise for a test.
	Meminfo string
}

var _ driver.Host = Host{}

func (h Host) Info(context.Context) (driver.HostInfo, error) {
	path := h.Meminfo
	if path == "" {
		path = "/proc/meminfo"
	}
	f, err := os.Open(path)
	if err != nil {
		return driver.HostInfo{}, err
	}
	defer func() { _ = f.Close() }()
	info := driver.HostInfo{CPUs: runtime.NumCPU()}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			info.MemoryTotal = kib << 10
		case "MemAvailable:":
			info.MemoryFree = kib << 10
		}
	}
	if err := sc.Err(); err != nil {
		return driver.HostInfo{}, err
	}
	if info.MemoryTotal == 0 {
		return driver.HostInfo{}, fmt.Errorf("%s says no MemTotal", path)
	}
	return info, nil
}
