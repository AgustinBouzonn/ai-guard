package sysres

import (
	"testing"
	"time"
)

func TestParseMeminfoUsesMemAvailableInKiB(t *testing.T) {
	const fixture = "MemTotal:        8215296 kB\nMemFree:          123456 kB\nMemAvailable:    7843116 kB\nBuffers:           10000 kB\nCached:           500000 kB\n"
	total, avail, ok := parseMeminfo([]byte(fixture))
	if !ok {
		t.Fatal("parseMeminfo reported failure on a valid /proc/meminfo")
	}
	if total != 8215296*1024 {
		t.Errorf("total = %d, want %d", total, uint64(8215296*1024))
	}
	if avail != 7843116*1024 {
		t.Errorf("avail = %d, want %d", avail, uint64(7843116*1024))
	}
}

func TestParseMeminfoFallsBackToFreePlusCacheOnOldKernels(t *testing.T) {
	// Kernels < 3.14 have no MemAvailable; free + buffers + cached is the usual estimate.
	const fixture = "MemTotal:        1000000 kB\nMemFree:          100000 kB\nBuffers:           20000 kB\nCached:           300000 kB\n"
	_, avail, ok := parseMeminfo([]byte(fixture))
	if !ok {
		t.Fatal("parseMeminfo reported failure")
	}
	if avail != 420000*1024 {
		t.Errorf("avail = %d, want %d", avail, uint64(420000*1024))
	}
}

func TestParseMeminfoRejectsGarbage(t *testing.T) {
	if _, _, ok := parseMeminfo([]byte("nonsense")); ok {
		t.Error("parseMeminfo accepted input without MemTotal")
	}
}

func TestMemoryReportsThisMachine(t *testing.T) {
	m, err := Memory()
	if err != nil {
		t.Fatalf("Memory(): %v", err)
	}
	if m.TotalBytes < 256<<20 {
		t.Errorf("TotalBytes = %d, implausibly small", m.TotalBytes)
	}
	if m.AvailBytes == 0 || m.AvailBytes > m.TotalBytes {
		t.Errorf("AvailBytes = %d outside (0, %d]", m.AvailBytes, m.TotalBytes)
	}
}

func TestCPUBusyIsAFractionOfAllCores(t *testing.T) {
	stop := make(chan struct{})
	go func() { // keep one core busy so the reading cannot be trivially zero
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	defer close(stop)
	busy, err := CPUBusy(150 * time.Millisecond)
	if err != nil {
		t.Skipf("CPUBusy unsupported here: %v", err)
	}
	if busy <= 0 || busy > 1 {
		t.Errorf("CPUBusy = %v, want a fraction in (0, 1]", busy)
	}
}
