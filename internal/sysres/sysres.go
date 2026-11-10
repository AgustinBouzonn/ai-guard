// Package sysres reads machine-wide memory and CPU headroom.
package sysres

import (
	"bytes"
	"strconv"
)

// Mem is a point-in-time view of physical memory.
type Mem struct {
	TotalBytes uint64
	AvailBytes uint64 // memory that can be handed to new processes without swapping
}

// parseMeminfo extracts total and available memory (bytes) from /proc/meminfo.
// It lives here, untagged, so it is unit-tested on every platform.
func parseMeminfo(b []byte) (total, avail uint64, ok bool) {
	var free, buffers, cached uint64
	var haveAvail bool
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		i := bytes.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		f := bytes.Fields(line[i+1:])
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(string(f[0]), 10, 64)
		if err != nil {
			continue
		}
		v *= 1024 // /proc/meminfo reports kB (KiB)
		switch string(line[:i]) {
		case "MemTotal":
			total = v
		case "MemAvailable":
			avail, haveAvail = v, true
		case "MemFree":
			free = v
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		}
	}
	if total == 0 {
		return 0, 0, false
	}
	if !haveAvail {
		avail = free + buffers + cached
	}
	if avail > total {
		avail = total
	}
	return total, avail, true
}
