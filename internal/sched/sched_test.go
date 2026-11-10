package sched

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeMem struct{ mb atomic.Int64 }

func (f *fakeMem) read() (int, bool) { return int(f.mb.Load()), true }

func newMem(mb int) *fakeMem { f := &fakeMem{}; f.mb.Store(int64(mb)); return f }

// acquireAsync reports the outcome of an Acquire that may block.
func acquireAsync(g *Gate, needMB int, maxWait time.Duration) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := g.Acquire(context.Background(), needMB, maxWait)
		ch <- err
	}()
	return ch
}

func stillBlocked(ch <-chan error) bool {
	select {
	case <-ch:
		return false
	case <-time.After(120 * time.Millisecond):
		return true
	}
}

func mustAcquire(t *testing.T, g *Gate, needMB int) *Ticket {
	t.Helper()
	tk, err := g.Acquire(context.Background(), needMB, time.Second)
	if err != nil {
		t.Fatalf("Acquire(%d MiB): %v", needMB, err)
	}
	return tk
}

func TestGateLimitsConcurrentWorkers(t *testing.T) {
	g := New(2, 0, newMem(100000).read)
	a := mustAcquire(t, g, 10)
	mustAcquire(t, g, 10)
	third := acquireAsync(g, 10, 5*time.Second)
	if !stillBlocked(third) {
		t.Fatal("a third job was admitted although only 2 workers are allowed")
	}
	a.Release()
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("third job after a release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third job still blocked after a worker was released")
	}
}

func TestGateWaitsUntilMemoryFreesUp(t *testing.T) {
	mem := newMem(600)
	g := New(4, 500, mem.read)
	job := acquireAsync(g, 200, 5*time.Second) // 600 - 200 leaves 400 < 500 reserve
	if !stillBlocked(job) {
		t.Fatal("job admitted although it would eat into the free-memory reserve")
	}
	mem.mb.Store(800) // 800 - 200 = 600 >= 500
	select {
	case err := <-job:
		if err != nil {
			t.Fatalf("job after memory freed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job still blocked after memory became available")
	}
}

func TestGateGivesUpWhenMemoryNeverFrees(t *testing.T) {
	g := New(4, 500, newMem(600).read)
	start := time.Now()
	_, err := g.Acquire(context.Background(), 200, 150*time.Millisecond)
	if !errors.Is(err, ErrNoMemory) {
		t.Fatalf("err = %v, want ErrNoMemory", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond || waited > 2*time.Second {
		t.Errorf("gave up after %v, want about the 150ms wait budget", waited)
	}
}

func TestGateReservesBudgetOfJobsThatHaveNotAllocatedYet(t *testing.T) {
	// The OS still reports 1200 MiB free while two fresh 500 MiB jobs warm up.
	g := New(8, 100, newMem(1200).read)
	mustAcquire(t, g, 500) // 1200-500 = 700
	mustAcquire(t, g, 500) // 700-500 = 200 >= 100
	if _, err := g.Acquire(context.Background(), 500, 100*time.Millisecond); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("third 500 MiB job: err = %v, want ErrNoMemory (only 200 MiB are really left)", err)
	}
}

func TestGateDoesNotCountUsedMemoryTwice(t *testing.T) {
	mem := newMem(1200)
	g := New(8, 100, mem.read)
	first := mustAcquire(t, g, 500)
	// The first job now really holds its 500 MiB: the OS shows it, and so does the job.
	mem.mb.Store(700)
	first.Report(500)
	if _, err := g.Acquire(context.Background(), 500, 200*time.Millisecond); err != nil {
		t.Fatalf("second job: %v; 700 free - 500 needed leaves 200 >= 100", err)
	}
}

func TestGateReturnsReservationOnRelease(t *testing.T) {
	g := New(8, 100, newMem(700).read)
	first := mustAcquire(t, g, 500)
	second := acquireAsync(g, 500, 5*time.Second)
	if !stillBlocked(second) {
		t.Fatal("second job admitted while the first still reserves its budget")
	}
	first.Release()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second job after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second job still blocked after the reservation was returned")
	}
}

func TestGateAdmitsWhenMemoryIsUnknown(t *testing.T) {
	g := New(1, 4096, func() (int, bool) { return 0, false })
	if _, err := g.Acquire(context.Background(), 100000, 100*time.Millisecond); err != nil {
		t.Fatalf("err = %v, want admission when free memory cannot be read", err)
	}
}

func TestGateAcquireHonoursContext(t *testing.T) {
	g := New(1, 0, newMem(100000).read)
	mustAcquire(t, g, 10)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := g.Acquire(ctx, 10, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestGateReleaseIsIdempotent(t *testing.T) {
	g := New(1, 0, newMem(100000).read)
	a := mustAcquire(t, g, 10)
	a.Release()
	a.Release() // must not free a second slot
	mustAcquire(t, g, 10)
	extra := acquireAsync(g, 10, 5*time.Second)
	if !stillBlocked(extra) {
		t.Fatal("a double Release opened a second worker slot")
	}
}

func TestAutoWorkers(t *testing.T) {
	cases := []struct {
		cpus  int
		busy  float64
		limit int
		want  int
	}{
		{16, 0.50, 4, 4}, // 8 idle cores, capped at 4
		{4, 0.90, 4, 1},  // 0.4 idle cores: still one worker
		{8, 0.25, 0, 6},  // no cap: one per idle core
		{2, 0.00, 8, 2},
		{16, 1.00, 4, 1},
	}
	for _, c := range cases {
		if got := AutoWorkers(c.cpus, c.busy, c.limit); got != c.want {
			t.Errorf("AutoWorkers(%d, %.2f, %d) = %d, want %d", c.cpus, c.busy, c.limit, got, c.want)
		}
	}
}
