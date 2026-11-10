package runner

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	ntdll    = syscall.NewLazyDLL("ntdll.dll")

	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procK32GetProcessMemoryInfo   = kernel32.NewProc("K32GetProcessMemoryInfo")
	procCreateToolhelp32Snapshot  = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First             = kernel32.NewProc("Thread32First")
	procThread32Next              = kernel32.NewProc("Thread32Next")
	procOpenThread                = kernel32.NewProc("OpenThread")
	procResumeThread              = kernel32.NewProc("ResumeThread")
	procNtResumeProcess           = ntdll.NewProc("NtResumeProcess")
)

const (
	jobObjectBasicAccountingInformation = 1
	jobObjectBasicProcessIDList         = 3
	jobObjectExtendedLimitInformation   = 9

	jobObjectLimitJobMemory      = 0x00000200
	jobObjectLimitKillOnJobClose = 0x00002000

	createSuspended          = 0x00000004
	belowNormalPriorityClass = 0x00004000

	processAllAccess               = 0x1F0FFF
	processVMRead                  = 0x0010
	processQueryLimitedInformation = 0x1000

	th32csSnapThread    = 0x00000004
	threadSuspendResume = 0x0002

	killExitCode = 137
)

// The structs below mirror winnt.h; their sizes on amd64 are 64, 144 and 48.
type ioCounters struct {
	ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64
}

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimitInformation struct {
	Basic                 jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobBasicAccountingInformation struct {
	TotalUserTime             int64 // 100 ns units
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type jobProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIDList             [1024]uintptr
}

// processMemoryCountersEx2 is PROCESS_MEMORY_COUNTERS_EX2 (96 bytes). Older
// systems fill only the first 80 bytes (EX) and leave the tail zero.
type processMemoryCountersEx2 struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
	PrivateWorkingSetSize      uintptr
	SharedCommitUsage          uint64
}

type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

// winTree is a Job Object holding the command and everything it spawns. The
// job is the unit of accounting, capping and killing; closing its handle kills
// whatever is left, so nothing outlives us even if we crash.
type winTree struct {
	job     syscall.Handle // 0 when the run is detached or the job could not be set up
	root    syscall.Handle // the command's own process handle
	keep    bool           // leave survivors alone when the root exits
	capByte uint64
	commit  uint64 // committed bytes of the whole job at the last sample
}

// createJob makes a job object. Unless keep is set, closing its last handle
// kills every process still inside, so nothing outlives us even if we crash.
func createJob(capBytes uint64, keep bool) (syscall.Handle, error) {
	h, _, e := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateJobObjectW: %w", e)
	}
	var li jobExtendedLimitInformation
	if !keep {
		li.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	}
	if capBytes > 0 {
		li.Basic.LimitFlags |= jobObjectLimitJobMemory
		li.JobMemoryLimit = uintptr(capBytes) // committed memory of the whole job
	}
	r, _, e := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&li)), unsafe.Sizeof(li))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0, fmt.Errorf("SetInformationJobObject: %w", e)
	}
	return syscall.Handle(h), nil
}

// resumeProcess resumes a process created with CREATE_SUSPENDED. os/exec does
// not expose the primary thread handle, so use NtResumeProcess and fall back
// to walking the thread list.
func resumeProcess(h syscall.Handle, pid uint32) error {
	if procNtResumeProcess.Find() == nil {
		if status, _, _ := procNtResumeProcess.Call(uintptr(h)); status == 0 {
			return nil
		}
	}
	return resumeThreads(pid)
}

func resumeThreads(pid uint32) error {
	snap, _, e := procCreateToolhelp32Snapshot.Call(th32csSnapThread, 0)
	if syscall.Handle(snap) == syscall.InvalidHandle {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", e)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))
	var te threadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for r, _, _ := procThread32First.Call(snap, uintptr(unsafe.Pointer(&te))); r != 0; r, _, _ = procThread32Next.Call(snap, uintptr(unsafe.Pointer(&te))) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, _, _ := procOpenThread.Call(threadSuspendResume, 0, uintptr(te.ThreadID))
		if th == 0 {
			continue
		}
		if prev, _, _ := procResumeThread.Call(th); prev != 0xFFFFFFFF {
			resumed++
		}
		syscall.CloseHandle(syscall.Handle(th))
	}
	if resumed == 0 {
		return fmt.Errorf("no thread of process %d could be resumed", pid)
	}
	return nil
}

// ShellCommand runs a line through cmd.exe. The command line is passed
// verbatim: with /s, cmd strips exactly the outer pair of quotes and keeps the
// rest as typed, which is the only form that survives inner quotes.
func ShellCommand(line string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	cmd := exec.Command(comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + comspec + `" /d /s /c "` + line + `"`}
	return cmd
}

func startTree(cmd *exec.Cmd, s *Spec) (tree, error) {
	t := &winTree{keep: s.KeepLeftovers}
	var flags uint32
	if s.LowPriority {
		flags |= belowNormalPriorityClass // inherited by everything the tool spawns
	}
	if !s.Detached {
		if job, err := createJob(uint64(s.HardCapMB)<<20, t.keep); err == nil {
			t.job = job
			t.capByte = uint64(s.HardCapMB) << 20
			// Start frozen so the tool cannot spawn anything before it is in the job.
			flags |= createSuspended
		}
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= flags
	if err := cmd.Start(); err != nil {
		if t.job != 0 {
			syscall.CloseHandle(t.job)
		}
		return nil, err
	}
	pid := uint32(cmd.Process.Pid)
	h, err := syscall.OpenProcess(processAllAccess, false, pid)
	if err == nil {
		t.root = h
	}
	if flags&createSuspended != 0 {
		assigned := false
		if t.root != 0 {
			r, _, _ := procAssignProcessToJobObject.Call(uintptr(t.job), uintptr(t.root))
			assigned = r != 0
		}
		if !assigned { // run uncontained rather than not at all
			syscall.CloseHandle(t.job)
			t.job, t.capByte = 0, 0
		}
		if err := resumeProcess(t.root, pid); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			t.release()
			return nil, fmt.Errorf("runner: cannot resume %s: %w", cmd.Path, err)
		}
	}
	return t, nil
}

func (t *winTree) release() {
	if t.job != 0 {
		syscall.CloseHandle(t.job)
		t.job = 0
	}
	if t.root != 0 {
		syscall.CloseHandle(t.root)
		t.root = 0
	}
}

// memoryOf returns the working set split into private and shared bytes, the
// peak working set and the commit charge of an open process handle.
func memoryOf(h syscall.Handle) (private, shared, peak, commit uint64, ok bool) {
	var mc processMemoryCountersEx2
	mc.Cb = uint32(unsafe.Sizeof(mc))
	r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.Cb))
	if r == 0 { // pre-EX2 system: ask for the 80-byte EX layout
		mc = processMemoryCountersEx2{Cb: 80}
		r, _, _ = procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), 80)
	}
	if r == 0 {
		return 0, 0, 0, 0, false
	}
	ws, priv := uint64(mc.WorkingSetSize), uint64(mc.PrivateWorkingSetSize)
	if priv == 0 || priv > ws {
		priv = ws
	}
	commit = uint64(mc.PrivateUsage)
	if commit == 0 {
		commit = uint64(mc.PagefileUsage)
	}
	return priv, ws - priv, uint64(mc.PeakWorkingSetSize), commit, true
}

func (t *winTree) pids() []uint32 {
	var l jobProcessIDList
	r, _, _ := procQueryInformationJobObject.Call(uintptr(t.job), jobObjectBasicProcessIDList, uintptr(unsafe.Pointer(&l)), unsafe.Sizeof(l), 0)
	if r == 0 && l.NumberOfProcessIdsInList == 0 { // ERROR_MORE_DATA still fills the list
		return nil
	}
	n := int(l.NumberOfProcessIdsInList)
	if n > len(l.ProcessIDList) {
		n = len(l.ProcessIDList)
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = uint32(l.ProcessIDList[i])
	}
	return out
}

// rss sums the private working set of every process in the job and counts
// shared pages (DLLs, the image itself) once, like the Linux backend.
func (t *winTree) rss() uint64 {
	if t.job == 0 {
		priv, shared, _, _, _ := memoryOf(t.root)
		return priv + shared
	}
	var private, maxShared, commit uint64
	for _, pid := range t.pids() {
		h, err := syscall.OpenProcess(processQueryLimitedInformation|processVMRead, false, pid)
		if err != nil {
			if h, err = syscall.OpenProcess(processQueryLimitedInformation, false, pid); err != nil {
				continue
			}
		}
		priv, shared, _, c, ok := memoryOf(h)
		syscall.CloseHandle(h)
		if !ok {
			continue
		}
		private += priv
		commit += c
		if shared > maxShared {
			maxShared = shared
		}
	}
	t.commit = commit
	return private + maxShared
}

// capHit: a job memory limit does not kill anything. Commits start failing,
// and the tool then crashes on its own, or (observed with a Go child on
// Windows 11) hangs half-dead at the limit. Treat "pinned at the wall" as the
// kill condition so the outcome is deterministic.
func (t *winTree) capHit() bool {
	return t.capByte > 0 && t.commit >= t.capByte/100*95
}

func (t *winTree) kill() {
	if t.job != 0 {
		procTerminateJobObject.Call(uintptr(t.job), killExitCode)
		return
	}
	if t.root != 0 {
		syscall.TerminateProcess(t.root, killExitCode)
	}
}

func (t *winTree) finish(ps *os.ProcessState) (cpu time.Duration, peakHint uint64, hardKilled bool) {
	if t.root != 0 {
		_, _, peakHint, _, _ = memoryOf(t.root) // still readable after exit while we hold the handle
	}
	if t.job != 0 {
		var acct jobBasicAccountingInformation
		procQueryInformationJobObject.Call(uintptr(t.job), jobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&acct)), unsafe.Sizeof(acct), 0)
		cpu = time.Duration(acct.TotalUserTime+acct.TotalKernelTime) * 100
		if t.capByte > 0 && ps != nil && ps.ExitCode() != 0 {
			// Hitting the job limit makes allocations fail inside the tool; it
			// then dies on its own. Recognise that by the commit peak.
			var li jobExtendedLimitInformation
			procQueryInformationJobObject.Call(uintptr(t.job), jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&li)), unsafe.Sizeof(li), 0)
			hardKilled = uint64(li.PeakJobMemoryUsed) >= t.capByte/100*95
		}
		if !t.keep {
			procTerminateJobObject.Call(uintptr(t.job), killExitCode) // leftovers die with the command
		}
	} else if ps != nil {
		cpu = ps.UserTime() + ps.SystemTime()
	}
	t.release()
	return cpu, peakHint, hardKilled
}

func (t *winTree) hardCap() string {
	if t.job != 0 && t.capByte > 0 {
		return "jobobject"
	}
	return ""
}

// HardCapAvailable reports which OS mechanism can enforce Spec.HardCapMB here
// ("" when none is usable).
func HardCapAvailable() string { return "jobobject" }

// ProcessRSS returns the working set of an arbitrary process (a tool daemon,
// for instance); ok is false when it does not exist or has exited.
func ProcessRSS(pid int) (bytes uint64, ok bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation|processVMRead, false, uint32(pid))
	if err != nil {
		if h, err = syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)); err != nil {
			return 0, false
		}
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if syscall.GetExitCodeProcess(h, &code) != nil || code != 259 { // STILL_ACTIVE
		return 0, false
	}
	priv, shared, _, _, ok := memoryOf(h)
	return priv + shared, ok
}

// KillProcess terminates one process by pid.
func KillProcess(pid int) {
	const processTerminate = 0x0001
	if h, err := syscall.OpenProcess(processTerminate, false, uint32(pid)); err == nil {
		syscall.TerminateProcess(h, killExitCode)
		syscall.CloseHandle(h)
	}
}
