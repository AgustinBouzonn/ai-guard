// Package sched admits work under two limits at once: a worker cap and the
// machine's free RAM.
package sched

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoMemory means the job could not be admitted within its wait budget
// because the machine never had enough free RAM for it.
var ErrNoMemory = errors.New("sched: not enough free memory")

// MemReader returns the RAM currently available to new processes, in MiB.
// ok=false means "unknown" and disables memory gating.
type MemReader func() (availMB int, ok bool)

// Gate is a memory-aware semaphore. A job is admitted when a worker slot is
// free and, after setting its budget aside, the machine keeps minFreeMB spare.
type Gate struct {
	mu         sync.Mutex
	maxWorkers int
	minFreeMB  int
	mem        MemReader
	running    map[*Ticket]struct{}
	changed    chan struct{} // closed and replaced whenever a ticket is released
}

// Ticket is one admitted job.
type Ticket struct {
	g        *Gate
	budgetMB int
	rssMB    atomic.Int64
	released atomic.Bool
}

// New builds a gate. maxWorkers < 1 is treated as 1.
func New(maxWorkers, minFreeMB int, mem MemReader) *Gate {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &Gate{maxWorkers: maxWorkers, minFreeMB: minFreeMB, mem: mem, running: map[*Ticket]struct{}{}, changed: make(chan struct{})}
}

const memPoll = 25 * time.Millisecond

// Acquire blocks until the job fits. Queueing behind other workers is bounded
// only by ctx; waiting for RAM while a slot is free is bounded by maxWait and
// ends in ErrNoMemory.
func (g *Gate) Acquire(ctx context.Context, needMB int, maxWait time.Duration) (*Ticket, error) {
	var memWait <-chan time.Time
	for {
		g.mu.Lock()
		slotFree := len(g.running) < g.maxWorkers
		if slotFree && g.fits(needMB) {
			t := &Ticket{g: g, budgetMB: needMB}
			g.running[t] = struct{}{}
			g.mu.Unlock()
			return t, nil
		}
		changed := g.changed
		g.mu.Unlock()

		var poll <-chan time.Time
		if slotFree { // blocked on memory only: nothing will signal us, so poll
			if memWait == nil {
				memWait = time.After(maxWait)
			}
			poll = time.After(memPoll)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-memWait:
			return nil, ErrNoMemory
		case <-changed:
		case <-poll:
		}
	}
}

// fits reports whether needMB can be set aside. Memory a running job has not
// touched yet is still "free" to the OS, so the unspent part of every budget
// is subtracted; what a job already uses is visible in the OS figure and must
// not be subtracted again.
func (g *Gate) fits(needMB int) bool {
	avail, ok := g.mem()
	if !ok {
		return true
	}
	for t := range g.running {
		if unspent := t.budgetMB - int(t.rssMB.Load()); unspent > 0 {
			avail -= unspent
		}
	}
	return avail-needMB >= g.minFreeMB
}

// Report records the job's current memory use (MiB), as sampled by the runner.
func (t *Ticket) Report(rssMB int) { t.rssMB.Store(int64(rssMB)) }

// Release frees the worker slot and the reservation. Safe to call repeatedly.
func (t *Ticket) Release() {
	if !t.released.CompareAndSwap(false, true) {
		return
	}
	g := t.g
	g.mu.Lock()
	delete(g.running, t)
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

// AutoWorkers sizes the pool from idle cores: one worker per idle logical
// core, at least one, at most limit (limit <= 0 means uncapped).
func AutoWorkers(numCPU int, busy float64, limit int) int {
	n := int(math.Round(float64(numCPU) * (1 - busy)))
	if limit > 0 && n > limit {
		n = limit
	}
	if n < 1 {
		n = 1
	}
	return n
}
