package kernel_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
)

func TestCloseStopsSpinningGuest(t *testing.T) {
	k := newKernelConfig(t, kernel.Config{Preempt: true}, "spin")
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/spin", Args: []string{"forever"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), scaled(5*time.Second))
	defer cancel()
	start := time.Now()
	if err := k.Close(ctx); err != nil {
		t.Fatalf("Close with a spinning guest: %v", err)
	}
	if st := p.Wait(); !st.Signaled() || st.Signal() != 9 {
		t.Fatalf("status %#x", st)
	}
	if d := time.Since(start); !timeScaled(t, "1 s kill latency") && d > time.Second {
		t.Fatalf("Close took %v", d)
	}
}

// TestGuestComputeDoesNotStallGC checks that with Config.Preempt, a guest
// computing without host calls does not hold up Go's stop-the-world GC.
// The guest spins until the test's cleanup kills it.
func TestGuestComputeDoesNotStallGC(t *testing.T) {
	if scaled(1) > 1 {
		t.Skip("PGMEM_TIME_SCALE > 1: the 500 ms GC bound is the point of this test, so it is not scaled")
	}
	k := newKernelConfig(t, kernel.Config{Preempt: true}, "spin")
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/spin", Args: []string{"forever"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Signal(9)
		p.Wait()
	})
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	runtime.GC()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("runtime.GC took %v while a guest computed", d)
	}
}

// TestSignalStopsSpinningGuest checks that Process.Signal ends a guest
// that computes without host calls, by preemption. spin has no dispatch
// export, so the host carries out SIGTERM's default action as it does
// SIGKILL's. Both end it "killed by" that signal.
func TestSignalStopsSpinningGuest(t *testing.T) {
	for _, sig := range []int{15, 9} {
		t.Run(fmt.Sprint(sig), func(t *testing.T) {
			k := newKernelConfig(t, kernel.Config{Preempt: true}, "spin")
			p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/spin", Args: []string{"forever"}})
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			if err := p.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan kernel.Status, 1)
			go func() { done <- p.Wait() }()
			select {
			case st := <-done:
				if !st.Signaled() || st.Signal() != sig {
					t.Fatalf("status %#x, want killed by %d", st, sig)
				}
			case <-time.After(closeTimeout):
				t.Fatalf("signal %d did not stop the spinning guest", sig)
			}
		})
	}
}

// Without preemption, a guest that polls like CHECK_FOR_INTERRUPTS, with a
// host call every 4096th check, is killed promptly and does not stall the
// GC.
func TestYieldingGuestKilledWithoutPreemption(t *testing.T) {
	k := newKernel(t, "spin") // preemption off, the default
	out := newReadyWriter()
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/spin", Args: []string{"yield"}, Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-out.ready: // it is entering the loop
	case <-time.After(closeTimeout):
		t.Fatal("spin yield never got ready")
	}
	t0 := time.Now()
	runtime.GC()
	if d := time.Since(t0); !timeScaled(t, "500 ms GC") && d > 500*time.Millisecond {
		t.Errorf("GC took %v with a yielding guest running", d)
	}
	t0 = time.Now()
	if err := p.Signal(9); err != nil {
		t.Fatal(err)
	}
	st := p.Wait()
	if !st.Signaled() || st.Signal() != 9 {
		t.Fatalf("status %#x", st)
	}
	if d := time.Since(t0); !timeScaled(t, "1 s kill latency") && d > time.Second {
		t.Fatalf("kill took %v", d)
	}
}
