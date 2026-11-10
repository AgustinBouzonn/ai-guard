//go:build !linux && !windows

package sysres

import "time"

// Memory is not implemented outside Linux and Windows; callers treat the error
// as "headroom unknown" and skip RAM gating.
func Memory() (Mem, error) { return Mem{}, errUnsupported }

// CPUBusy is not implemented outside Linux and Windows.
func CPUBusy(time.Duration) (float64, error) { return 0, errUnsupported }
