package kernel_test

import (
	"context"
	"io"
	"testing"

	"github.com/feverenergy/pgmem/internal/kernel"
)

func TestSemFixture(t *testing.T) {
	k := newKernel(t, "sem")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/sem"})
	if !st.Exited() || st.ExitCode() != 0 || out != "sem ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}

// A process killed while blocked in sem_wait dies promptly and its
// semaphore goes with it.
func TestSemKilledWaiter(t *testing.T) {
	k := newKernel(t, "sem")
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/sem", Args: []string{"blocker"}, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 'r' {
		t.Fatalf("ready: %q %v", b, err)
	}
	if err := p.Signal(9); err != nil {
		t.Fatal(err)
	}
	if st := p.Wait(); !st.Signaled() || st.Signal() != 9 {
		t.Fatalf("status %#x", st)
	}
	if n := kernel.SemCount(k); n != 0 {
		t.Fatalf("%d semaphores left", n)
	}
}

// A keyed segment is never removed. It holds a semaphore, and a process
// that is still running has it attached. This is the postmaster and its
// main segment at shutdown. Kernel.Close kills the process and frees the
// segment and the semaphore with it.
func TestCloseFreesKeptSegmentAndSemaphore(t *testing.T) {
	k := newKernel(t, "sem")
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/sem", Args: []string{"keeper"}, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 'r' {
		t.Fatalf("ready: %q %v", b, err)
	}
	if segs, att := kernel.ShmStats(k); segs != 1 || att != 1 {
		t.Fatalf("while kept: %d segments, %d attachments", segs, att)
	}
	if n := kernel.SemCount(k); n != 1 {
		t.Fatalf("while kept: %d semaphores", n)
	}
	closeKernel(t, k)
	if st := p.Wait(); !st.Signaled() || st.Signal() != 9 {
		t.Fatalf("status %#x", st)
	}
	checkFreed(t, k)
}
