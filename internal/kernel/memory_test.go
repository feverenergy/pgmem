package kernel_test

import (
	"context"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
)

// startPausers starts n "pauser" processes and returns once each has said
// it is ready and had a moment to block in pause().
func startPausers(t *testing.T, k *kernel.Kernel, n int) []*kernel.Process {
	t.Helper()
	ps := make([]*kernel.Process, n)
	for i := range ps {
		out := newReadyWriter()
		p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/signals", Args: []string{"pauser"}, Stdout: out})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-out.ready:
		case <-time.After(closeTimeout):
			t.Fatal("pauser never got ready")
		}
		ps[i] = p
	}
	time.Sleep(5 * time.Millisecond)
	return ps
}

// TestKilledBlockedProcessesFreeMemory kills processes blocked in a host
// call, one by one with SIGKILL and all at once with Close, and checks that
// every one's memory is freed. With preemption on, wazero's context watcher
// can close the module before the host call does. That defers the free to
// a path the panicking host call skips. See Kernel.exit. The counts are
// small, because each pauser commits about 9 MiB.
func TestKilledBlockedProcessesFreeMemory(t *testing.T) {
	const kills, closes = 20, 10
	for _, preempt := range []bool{true, false} {
		name := "preemption"
		if !preempt {
			name = "nopreemption"
		}
		t.Run(name+"/SIGKILL", func(t *testing.T) {
			k := newKernelConfig(t, kernel.Config{Preempt: preempt}, "signals")
			for range kills {
				p := startPausers(t, k, 1)[0]
				if err := p.Signal(9); err != nil {
					t.Fatal(err)
				}
				if st := p.Wait(); st != kernel.KilledStatus {
					t.Fatalf("status %#x, want SIGKILL", st)
				}
			}
			st := kernel.MemoryStats(k)
			t.Logf("after %d SIGKILLs: %d live, %d bytes committed", kills, st.Live, st.Committed)
			if st.Live != 0 || st.Committed != 0 {
				t.Fatalf("%d of %d killed processes' memories not freed (%d bytes committed)", st.Live, kills, st.Committed)
			}
		})
		t.Run(name+"/Close", func(t *testing.T) {
			k := newKernelConfig(t, kernel.Config{Preempt: preempt}, "signals")
			ps := startPausers(t, k, closes)
			closeKernel(t, k)
			for _, p := range ps {
				if st := p.Wait(); st != kernel.KilledStatus {
					t.Fatalf("status %#x, want SIGKILL", st)
				}
			}
			st := kernel.MemoryStats(k)
			t.Logf("after Close of %d: %d live, %d bytes committed", closes, st.Live, st.Committed)
			if st.Live != 0 || st.Committed != 0 {
				t.Fatalf("%d of %d processes' memories not freed at Close (%d bytes committed)", st.Live, closes, st.Committed)
			}
		})
	}
}
