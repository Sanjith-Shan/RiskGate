package main

import (
	"syscall"
	"unsafe"
)

var procGetSystemTimes = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemTimes")

// cpuTimes returns idle and total CPU time in 100 ns units, summed over all
// processors. Kernel time includes idle time.
func cpuTimes() (idle, total uint64) {
	var i, k, u syscall.Filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0
	}
	ft := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return ft(i), ft(k) + ft(u)
}
