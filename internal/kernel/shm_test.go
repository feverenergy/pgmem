package kernel_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
)

func TestShmFixture(t *testing.T) {
	k := newKernel(t, "shm")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/shm"})
	if !st.Exited() || st.ExitCode() != 0 || out != "shm ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}

// A process killed while attached to a removed segment releases it, and
// the segment is freed.
func TestShmKilledHolderFreesSegment(t *testing.T) {
	k := newKernel(t, "shm")
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/shm", Args: []string{"holder"}, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 'r' {
		t.Fatalf("ready: %q %v", b, err)
	}
	if segs, att := kernel.ShmStats(k); segs != 1 || att != 1 {
		t.Fatalf("while held: %d segments, %d attachments", segs, att)
	}
	if err := p.Signal(9); err != nil {
		t.Fatal(err)
	}
	if st := p.Wait(); !st.Signaled() || st.Signal() != 9 {
		t.Fatalf("status %#x", st)
	}
	if segs, att := kernel.ShmStats(k); segs != 0 || att != 0 {
		t.Fatalf("after kill: %d segments, %d attachments", segs, att)
	}
}

// A process killed while computing without host calls releases a removed
// segment it has attached. Preemption ends it, not a host call. The
// segment is freed, and so are the memory and its overlay (checkFreed).
func TestShmKilledSpinningHolderFreesSegment(t *testing.T) {
	k := newKernelConfig(t, kernel.Config{Preempt: true}, "shm")
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/shm", Args: []string{"holder-spin"}, Stdout: w})
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
	done := make(chan kernel.Status, 1)
	go func() { done <- p.Wait() }()
	select {
	case st := <-done:
		if !st.Signaled() || st.Signal() != 9 {
			t.Fatalf("status %#x", st)
		}
	case <-time.After(closeTimeout):
		t.Fatal("SIGKILL did not stop the spinning holder")
	}
	if segs, att := kernel.ShmStats(k); segs != 0 || att != 0 {
		t.Fatalf("after kill: %d segments, %d attachments", segs, att)
	}
	if st := kernel.MemoryStats(k); st.Live != 0 || st.Overlays != 0 {
		t.Fatalf("after kill: %d memories, %d overlays", st.Live, st.Overlays)
	}
}
