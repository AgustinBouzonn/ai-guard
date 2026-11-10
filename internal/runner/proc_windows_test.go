package runner

import "syscall"

func pidAlive(pid int) bool {
	const synchronize = 0x00100000
	h, err := syscall.OpenProcess(synchronize, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	ev, _ := syscall.WaitForSingleObject(h, 0)
	return ev == syscall.WAIT_TIMEOUT
}

func killPid(pid int) {
	const terminate = 0x0001
	if h, err := syscall.OpenProcess(terminate, false, uint32(pid)); err == nil {
		syscall.TerminateProcess(h, 1)
		syscall.CloseHandle(h)
	}
}
