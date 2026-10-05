//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
)

// cpuTimes returns idle and total CPU time in jiffies from /proc/stat (zero
// where there is none, which reports 0% busy).
func cpuTimes() (idle, total uint64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	for i, s := range f[1:] {
		n, _ := strconv.ParseUint(s, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return idle, total
}
