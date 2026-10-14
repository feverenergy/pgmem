package kernel_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
)

func TestSignalsFixture(t *testing.T) {
	k := newKernel(t, "signals")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/signals"})
	if !st.Exited() || st.ExitCode() != 0 || out != "signals ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}

func TestSignalFromGo(t *testing.T) {
	k := newKernel(t, "signals")
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/signals", Args: []string{"pauser"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := p.Signal(15); err != nil {
		t.Fatal(err)
	}
	st := p.Wait()
	if !st.Signaled() || st.Signal() != 15 {
		t.Fatalf("status %#x", st)
	}
	if err := p.Signal(0); err != kernel.ErrProcessDone {
		t.Fatalf("Signal after exit: %v", err)
	}
}

// A signal sent before the guest has registered its signal block is held
// and delivered once it registers.
func TestSignalBeforeRegistration(t *testing.T) {
	k := newKernel(t, "signals")
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/signals", Args: []string{"pauser"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(15); err != nil {
		t.Fatal(err)
	}
	if st := p.Wait(); !st.Signaled() || st.Signal() != 15 {
		t.Fatalf("status %#x", st)
	}
}

// Senders racing a process's exit never write into its freed memory. Each
// sender keeps signalling until Signal reports the process done, or until
// the iteration cap. -race checks the handover.
func TestSignalStormWhileExiting(t *testing.T) {
	const procs, senders, maxSends = 4, 3, 100000
	k := newKernel(t, "signals")
	for range procs {
		p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/signals", Args: []string{"pauser"}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		for s := range senders {
			go func() {
				defer func() { done <- struct{}{} }()
				// SIGCHLD is ignored, so it is read and then dropped.
				// SIGUSR1 kills.
				sig := 17
				if s == 0 {
					sig = 10
				}
				for range maxSends {
					if p.Signal(sig) == kernel.ErrProcessDone {
						return
					}
				}
			}()
		}
		for range senders {
			<-done
		}
		if st := p.Wait(); !st.Signaled() || st.Signal() != 10 {
			t.Fatalf("status %#x", st)
		}
	}
}

// A SIGKILL racing the guest's own signal_exit still ends the process
// "signaled", by whichever signal won.
func TestSignalKillRacingSignalExit(t *testing.T) {
	k := newKernel(t, "signals")
	for range 10 {
		p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/signals", Args: []string{"pauser"}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		p.Signal(15)
		p.Signal(9)
		if st := p.Wait(); !st.Signaled() || (st.Signal() != 15 && st.Signal() != 9) {
			t.Fatalf("status %#x", st)
		}
	}
}

// TestBareSleepInterrupted checks that a program that calls no timer
// function of the compat layer still gets its clock_nanosleep. A signal
// then interrupts its nanosleep with EINTR. wasi-libc's would fail with
// ENOTSUP.
func TestBareSleepInterrupted(t *testing.T) {
	k := newKernel(t, "bare")
	out := newReadyWriter()
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/bare", Args: []string{"sleep"}, Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-out.ready:
	case <-time.After(closeTimeout):
		t.Fatal("never got ready")
	}
	time.Sleep(5 * time.Millisecond)
	if err := p.Signal(10); err != nil {
		t.Fatal(err)
	}
	if st := p.Wait(); !st.Exited() || st.ExitCode() != 0 {
		t.Fatalf("status %#x, want exit 0", st)
	}
}

// Kernel.Signal signals any live process by pid. ProcCount counts the
// processes that have not exited.
func TestKernelSignalAndProcCount(t *testing.T) {
	k := newKernel(t, "signals")
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/signals", Args: []string{"pauser"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := k.ProcCount(); n != 1 {
		t.Fatalf("ProcCount %d before the signal, want 1", n)
	}
	if err := k.Signal(p.PID(), 15); err != nil {
		t.Fatal(err)
	}
	if st := p.Wait(); !st.Signaled() || st.Signal() != 15 {
		t.Fatalf("status %#x", st)
	}
	if n := k.ProcCount(); n != 0 {
		t.Fatalf("ProcCount %d after Wait, want 0", n)
	}
	if err := k.Signal(p.PID(), 0); err != kernel.ErrProcessDone {
		t.Fatalf("Signal to an exited pid: %v", err)
	}
	if err := k.Signal(99999, 0); err != kernel.ErrProcessDone {
		t.Fatalf("Signal to an unknown pid: %v", err)
	}
}

// An interval timer re-arms. setitimer with a 20 ms interval delivers at
// least 3 SIGALRMs within 200 ms. The child waits up to 2 s and reports how
// long it took, so a slow machine fails on the bound, not on a hang.
func TestIntervalTimerRearms(t *testing.T) {
	k := newKernel(t, "signals")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/signals", Args: []string{"itimer"}})
	if !st.Exited() || st.ExitCode() != 0 {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
	ms, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if limit := scaled(200 * time.Millisecond); time.Duration(ms)*time.Millisecond > limit {
		t.Fatalf("3 SIGALRMs took %d ms, want at most %v", ms, limit)
	}
}

// A deterministic kernel never fires the interval timer. The itimer
// fixture sees no SIGALRM in its 2 s and exits 91.
func TestDeterministicTimerNeverFires(t *testing.T) {
	k := newKernelConfig(t, kernel.Config{Deterministic: true}, "signals")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/signals", Args: []string{"itimer"}})
	if !st.Exited() || st.ExitCode() != 91 {
		t.Fatalf("status %#x, want exit 91\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}
