package sysres

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
)

// memoryStatusEx mirrors MEMORYSTATUSEX (64 bytes).
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// Memory reports physical RAM. Available is capped by the remaining commit
// charge: once the commit limit is reached Windows refuses allocations even if
// standby pages exist, so that is the real headroom for a new process.
func Memory() (Mem, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return Mem{}, fmt.Errorf("GlobalMemoryStatusEx: %w", e)
	}
	avail := m.AvailPhys
	if m.AvailPageFile < avail {
		avail = m.AvailPageFile
	}
	return Mem{TotalBytes: m.TotalPhys, AvailBytes: avail}, nil
}

func systemTimes() (idle, total uint64, err error) {
	var i, k, u syscall.Filetime
	r, _, e := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, fmt.Errorf("GetSystemTimes: %w", e)
	}
	ft := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return ft(i), ft(k) + ft(u), nil // kernel time already includes idle time
}

// CPUBusy samples system-wide CPU utilisation over window and returns the busy
// fraction across all logical cores (0..1).
func CPUBusy(window time.Duration) (float64, error) {
	i0, t0, err := systemTimes()
	if err != nil {
		return 0, err
	}
	time.Sleep(window)
	i1, t1, err := systemTimes()
	if err != nil {
		return 0, err
	}
	if t1 <= t0 {
		return 0, nil
	}
	busy := float64(t1-t0) - float64(i1-i0)
	if busy < 0 {
		busy = 0
	}
	return busy / float64(t1-t0), nil
}
