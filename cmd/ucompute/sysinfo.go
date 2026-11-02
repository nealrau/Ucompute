package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var (
	memOnce  sync.Once
	memTotal uint64
)

// totalMemory returns physical memory in bytes, or 0 if unknown.
func totalMemory() uint64 {
	memOnce.Do(func() {
		switch runtime.GOOS {
		case "darwin":
			if out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output(); err == nil {
				memTotal, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			}
		case "linux":
			b, err := os.ReadFile("/proc/meminfo")
			if err != nil {
				return
			}
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
					kb, _ := strconv.ParseUint(f[1], 10, 64)
					memTotal = kb << 10
				}
			}
		default:
			memTotal = platformMemory()
		}
	})
	return memTotal
}
