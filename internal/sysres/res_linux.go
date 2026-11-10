package sysres

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"time"
)

// Memory reads /proc/meminfo (MemAvailable already discounts reclaimable cache).
func Memory() (Mem, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Mem{}, err
	}
	total, avail, ok := parseMeminfo(b)
	if !ok {
		return Mem{}, errors.New("sysres: cannot parse /proc/meminfo")
	}
	return Mem{TotalBytes: total, AvailBytes: avail}, nil
}

func cpuTicks() (busy, total uint64, err error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		nl = len(b)
	}
	f := bytes.Fields(b[:nl]) // cpu user nice system idle iowait irq softirq steal ...
	if len(f) < 5 || string(f[0]) != "cpu" {
		return 0, 0, errors.New("sysres: unexpected /proc/stat")
	}
	for i, s := range f[1:] {
		if i >= 8 { // guest/guest_nice are already included in user/nice
			break
		}
		v, perr := strconv.ParseUint(string(s), 10, 64)
		if perr != nil {
			return 0, 0, perr
		}
		total += v
		if i != 3 && i != 4 { // not idle, not iowait
			busy += v
		}
	}
	return busy, total, nil
}

// CPUBusy samples system-wide CPU utilisation over window and returns the busy
// fraction across all logical cores (0..1).
func CPUBusy(window time.Duration) (float64, error) {
	b0, t0, err := cpuTicks()
	if err != nil {
		return 0, err
	}
	time.Sleep(window)
	b1, t1, err := cpuTicks()
	if err != nil {
		return 0, err
	}
	if t1 <= t0 {
		return 0, nil
	}
	return float64(b1-b0) / float64(t1-t0), nil
}
