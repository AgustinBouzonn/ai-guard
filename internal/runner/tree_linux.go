package runner

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// linuxTree tracks a process group. Membership is "same process group as the
// root" or "descends from the root" (which also catches children that moved
// to a group of their own); a daemon that re-parents itself to init escapes
// on purpose.
type linuxTree struct {
	pid       int // root pid; also the pgid when contained
	contained bool
	keep      bool // leave survivors alone when the root exits
	known     map[int]struct{}
	samples   int
	page      uint64
	cg        *cgroup
}

// gateScript parks the child until the parent has finished placing it in its
// cgroup and applying rlimits, so nothing it spawns can escape either.
const gateScript = `read -r _ <&3; exec 3<&-; exec "$@"`

func startTree(cmd *exec.Cmd, s *Spec) (tree, error) {
	t := &linuxTree{contained: !s.Detached, keep: s.KeepLeftovers, known: map[int]struct{}{}, page: uint64(os.Getpagesize())}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: t.contained}

	// A cgroup cannot be removed while it has members, so a hard cap implies cleanup.
	wantCap := s.HardCapMB > 0 && t.contained && !t.keep && cgroupBase() != nil
	var gate *os.File
	if (wantCap || s.RlimitASMB > 0) && cmd.Err == nil {
		if sh, err := exec.LookPath("sh"); err == nil {
			if r, w, err := os.Pipe(); err == nil {
				cmd.Args = append([]string{"sh", "-c", gateScript, "aiguard-gate", cmd.Path}, cmd.Args[1:]...)
				cmd.Path = sh
				cmd.ExtraFiles = []*os.File{r} // fd 3 in the child
				gate = w
				defer r.Close()
			}
		}
	}
	if err := cmd.Start(); err != nil {
		if gate != nil {
			gate.Close()
		}
		return nil, err
	}
	t.pid = cmd.Process.Pid
	if gate != nil {
		if wantCap {
			if cg, err := newCgroup(s.HardCapMB); err == nil {
				if cg.add(t.pid) == nil {
					t.cg = cg
				} else {
					cg.remove()
				}
			}
		}
		if s.RlimitASMB > 0 {
			prlimitAS(t.pid, uint64(s.RlimitASMB)<<20)
		}
		gate.Write([]byte{'\n'})
		gate.Close()
	}
	if s.LowPriority {
		which := syscall.PRIO_PROCESS
		if t.contained {
			which = syscall.PRIO_PGRP
		}
		syscall.Setpriority(which, t.pid, 10)
	}
	return t, nil
}

// ShellCommand runs a line through the POSIX shell.
func ShellCommand(line string) *exec.Cmd { return exec.Command("sh", "-c", line) }

type rlimit64 struct{ Cur, Max uint64 }

func prlimitAS(pid int, bytes uint64) error {
	lim := rlimit64{bytes, bytes}
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), uintptr(syscall.RLIMIT_AS), uintptr(unsafe.Pointer(&lim)), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// readStat returns ppid and pgrp of pid. comm may contain spaces and
// parentheses, so fields are counted from the last ')'.
func readStat(pid int) (ppid, pgrp int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := bytes.Fields(b[i+1:]) // state ppid pgrp ...
	if len(f) < 3 {
		return 0, 0, false
	}
	ppid, e1 := strconv.Atoi(string(f[1]))
	pgrp, e2 := strconv.Atoi(string(f[2]))
	return ppid, pgrp, e1 == nil && e2 == nil
}

// readStatm returns resident and file-backed/shared pages of pid.
func readStatm(pid int) (resident, shared uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0, 0, false
	}
	f := bytes.Fields(b) // size resident shared ...
	if len(f) < 3 {
		return 0, 0, false
	}
	resident, e1 := strconv.ParseUint(string(f[1]), 10, 64)
	shared, e2 := strconv.ParseUint(string(f[2]), 10, 64)
	return resident, shared, e1 == nil && e2 == nil
}

// scan rebuilds the member set. PIDs are handed out in increasing order, so a
// cheap pass only looks at processes younger than the root; every tenth pass
// (and every pass that precedes a kill) reads all of /proc to stay correct
// across PID wrap-around.
func (t *linuxTree) scan(full bool) {
	d, err := os.Open("/proc")
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	type proc struct{ pid, ppid, pgrp int }
	procs := make([]proc, 0, 32)
	for _, n := range names {
		if n[0] < '0' || n[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		if !full && pid < t.pid {
			if _, seen := t.known[pid]; !seen {
				continue
			}
		}
		if ppid, pgrp, ok := readStat(pid); ok {
			procs = append(procs, proc{pid, ppid, pgrp})
		}
	}
	members := map[int]struct{}{t.pid: {}}
	for grew := true; grew; {
		grew = false
		for _, p := range procs {
			if _, in := members[p.pid]; in {
				continue
			}
			if _, childOfMember := members[p.ppid]; childOfMember || (t.contained && p.pgrp == t.pid) {
				members[p.pid] = struct{}{}
				grew = true
			}
		}
	}
	t.known = members
}

// rss sums private (anonymous) pages of every member and counts file-backed
// pages once: N copies of the same compiler share one mapping of its binary,
// so adding their full RSS would overstate what the tree costs the machine.
func (t *linuxTree) rss() uint64 {
	t.scan(t.samples%10 == 0)
	t.samples++
	var private, maxShared uint64
	for pid := range t.known {
		res, shared, ok := readStatm(pid)
		if !ok {
			continue
		}
		if shared > res {
			shared = res
		}
		private += res - shared
		if shared > maxShared {
			maxShared = shared
		}
	}
	return (private + maxShared) * t.page
}

// capHit: once the kernel has OOM-killed inside our cgroup the run is void;
// v1 kills a single victim and would let the rest limp on.
func (t *linuxTree) capHit() bool { return t.cg != nil && t.cg.oomKilled() }

func (t *linuxTree) kill() {
	t.scan(true) // fresh membership: never signal a recycled PID
	if t.contained {
		syscall.Kill(-t.pid, syscall.SIGKILL)
	}
	for pid := range t.known {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	if t.cg != nil {
		t.cg.kill()
	}
}

func (t *linuxTree) finish(ps *os.ProcessState) (cpu time.Duration, peakHint uint64, hardKilled bool) {
	if t.contained && !t.keep {
		// Leftovers in the group die with the command. A group id cannot be
		// reused while it has members, and if it is already empty this is ESRCH.
		syscall.Kill(-t.pid, syscall.SIGKILL)
	}
	if ps != nil {
		cpu = ps.UserTime() + ps.SystemTime() // includes reaped descendants
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			peakHint = uint64(ru.Maxrss) << 10 // KiB; largest single process
		}
	}
	if t.cg != nil {
		hardKilled = t.cg.oomKilled()
		t.cg.kill()
		t.cg.remove()
	}
	return cpu, peakHint, hardKilled
}

func (t *linuxTree) hardCap() string {
	if t.cg == nil {
		return ""
	}
	return t.cg.name()
}

// HardCapAvailable reports which OS mechanism can enforce Spec.HardCapMB here
// ("" when none is usable).
func HardCapAvailable() string {
	if b := cgroupBase(); b != nil {
		return "cgroup" + strconv.Itoa(b.version)
	}
	return ""
}

// ProcessRSS returns the resident memory of an arbitrary process (a tool
// daemon, for instance); ok is false when it does not exist.
func ProcessRSS(pid int) (bytes uint64, ok bool) {
	res, _, ok := readStatm(pid)
	return res * uint64(os.Getpagesize()), ok && res > 0
}

// KillProcess terminates one process by pid.
func KillProcess(pid int) {
	if pid > 1 {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
