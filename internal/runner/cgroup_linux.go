package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// cgroupParent is a directory in which this process may create memory-limited
// child cgroups: a delegated cgroup v2 subtree (systemd user session), or a
// writable v1 memory hierarchy (root in a container, hybrid WSL2 as root).
type cgroupParent struct {
	version int // 1 or 2
	dir     string
}

var (
	cgOnce   sync.Once
	cgParent *cgroupParent
	cgSeq    atomic.Uint64
)

func cgroupBase() *cgroupParent {
	cgOnce.Do(func() { cgParent = detectCgroupParent("/proc/self/cgroup", "/proc/self/mountinfo") })
	return cgParent
}

type cgMount struct {
	root, point string
}

// parseCgroupMounts finds the cgroup2 mount and the v1 mount that carries the
// memory controller in a mountinfo file.
func parseCgroupMounts(mountinfo []byte) (v2, v1mem *cgMount) {
	for _, line := range bytes.Split(mountinfo, []byte{'\n'}) {
		sep := bytes.Index(line, []byte(" - "))
		if sep < 0 {
			continue
		}
		left, right := bytes.Fields(line[:sep]), bytes.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 3 {
			continue
		}
		m := &cgMount{root: string(left[3]), point: string(left[4])}
		switch string(right[0]) {
		case "cgroup2":
			if v2 == nil {
				v2 = m
			}
		case "cgroup":
			for _, o := range strings.Split(string(right[2]), ",") {
				if o == "memory" && v1mem == nil {
					v1mem = m
				}
			}
		}
	}
	return v2, v1mem
}

// ownCgroupPaths returns this process's path in the unified (v2) hierarchy
// and in the v1 memory hierarchy, as listed in /proc/self/cgroup.
func ownCgroupPaths(procCgroup []byte) (v2, v1mem string) {
	for _, line := range strings.Split(string(procCgroup), "\n") {
		p := strings.SplitN(line, ":", 3)
		if len(p) != 3 {
			continue
		}
		if p[0] == "0" && p[1] == "" {
			v2 = p[2]
			continue
		}
		for _, c := range strings.Split(p[1], ",") {
			if c == "memory" {
				v1mem = p[2]
			}
		}
	}
	return v2, v1mem
}

func (m *cgMount) dirFor(path string) string {
	if m.root != "/" { // mount shows a subtree: paths are relative to its root
		path = strings.TrimPrefix(path, m.root)
	}
	return filepath.Join(m.point, path)
}

func detectCgroupParent(procCgroup, mountinfo string) *cgroupParent {
	pc, err1 := os.ReadFile(procCgroup)
	mi, err2 := os.ReadFile(mountinfo)
	if err1 != nil || err2 != nil {
		return nil
	}
	v2m, v1m := parseCgroupMounts(mi)
	v2p, v1p := ownCgroupPaths(pc)
	if v2m != nil && v2p != "" {
		own := v2m.dirFor(v2p)
		// A sibling under our parent first (the usual delegated layout: the
		// parent has no processes of its own), then a child of our own group.
		for _, cand := range []string{filepath.Dir(own), own} {
			if !strings.HasPrefix(cand, v2m.point) {
				continue
			}
			sub, _ := os.ReadFile(filepath.Join(cand, "cgroup.subtree_control"))
			if hasField(string(sub), "memory") && probeCgroup(cand, "memory.max") {
				return &cgroupParent{version: 2, dir: cand}
			}
		}
	}
	if v1m != nil && v1p != "" {
		if own := v1m.dirFor(v1p); probeCgroup(own, "memory.limit_in_bytes") {
			return &cgroupParent{version: 1, dir: own}
		}
	}
	return nil
}

func hasField(s, want string) bool {
	for _, f := range strings.Fields(s) {
		if f == want {
			return true
		}
	}
	return false
}

// probeCgroup checks that we can create a child cgroup there and that the
// child exposes the memory control file.
func probeCgroup(parent, control string) bool {
	d := filepath.Join(parent, fmt.Sprintf("aiguard-probe-%d", os.Getpid()))
	if err := os.Mkdir(d, 0o755); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(d, control))
	os.Remove(d)
	return err == nil
}

type cgroup struct {
	version int
	dir     string
}

func newCgroup(capMB int) (*cgroup, error) {
	base := cgroupBase()
	if base == nil {
		return nil, os.ErrNotExist
	}
	c := &cgroup{version: base.version, dir: filepath.Join(base.dir, fmt.Sprintf("aiguard-%d-%d", os.Getpid(), cgSeq.Add(1)))}
	if err := os.Mkdir(c.dir, 0o755); err != nil {
		return nil, err
	}
	limit := strconv.FormatUint(uint64(capMB)<<20, 10)
	var err error
	if c.version == 2 {
		err = c.write("memory.max", limit)
		c.write("memory.swap.max", "0") // swapping out would defeat the cap; absent without swap accounting
		c.write("memory.oom.group", "1")
	} else {
		err = c.write("memory.limit_in_bytes", limit)
		c.write("memory.memsw.limit_in_bytes", limit)
	}
	if err != nil {
		os.Remove(c.dir)
		return nil, err
	}
	return c, nil
}

func (c *cgroup) name() string { return "cgroup" + strconv.Itoa(c.version) }

func (c *cgroup) write(file, val string) error {
	return os.WriteFile(filepath.Join(c.dir, file), []byte(val), 0o644)
}

func (c *cgroup) add(pid int) error { return c.write("cgroup.procs", strconv.Itoa(pid)) }

func (c *cgroup) pids() []int {
	b, _ := os.ReadFile(filepath.Join(c.dir, "cgroup.procs"))
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (c *cgroup) kill() {
	if c.version == 2 && c.write("cgroup.kill", "1") == nil {
		return
	}
	for _, pid := range c.pids() {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// counter reads "key N" from a flat keyed cgroup file.
func (c *cgroup) counter(file, key string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(c.dir, file))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			n, err := strconv.ParseUint(f[1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// oomKilled reports whether the kernel killed something in this cgroup for
// exceeding the cap.
func (c *cgroup) oomKilled() bool {
	if c.version == 2 {
		n, _ := c.counter("memory.events", "oom_kill")
		return n > 0
	}
	if n, ok := c.counter("memory.oom_control", "oom_kill"); ok {
		return n > 0
	}
	b, _ := os.ReadFile(filepath.Join(c.dir, "memory.failcnt")) // kernels < 4.13
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n > 0
}

// remove deletes the cgroup; members die asynchronously after SIGKILL, so the
// directory may stay busy for a moment.
func (c *cgroup) remove() {
	for i := 0; i < 100; i++ {
		if err := os.Remove(c.dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}
