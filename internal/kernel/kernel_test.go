package kernel_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasiabi"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

func newKernel(t *testing.T, fixtures ...string) *kernel.Kernel {
	t.Helper()
	return newKernelConfig(t, kernel.Config{}, fixtures...)
}

// newKernelConfig is newKernel with cfg. It fills in FS and CacheDir.
// The test's cleanup closes the kernel and fails the test if any process's
// memory was not freed.
func newKernelConfig(t *testing.T, cfg kernel.Config, fixtures ...string) *kernel.Kernel {
	t.Helper()
	ctx := context.Background()
	cache, err := wasmrt.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg.FS, cfg.CacheDir = memfs.New(), cache
	k, err := kernel.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeKernel(t, k)
		checkFreed(t, k)
	})
	for _, name := range fixtures {
		wasm, err := os.ReadFile("testdata/" + name + ".wasm")
		if err != nil {
			t.Fatalf("%v (run make fixtures)", err)
		}
		m, err := k.Compile(ctx, wasm)
		if err != nil {
			t.Fatal(err)
		}
		if err := k.Register("/fixtures/"+name, m); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// scaled multiplies a test's time bound by PGMEM_TIME_SCALE, which
// defaults to 1. make test-linux-amd64 runs under CPU emulation and sets
// it to 10. When timeScaled reports true, a test skips a bound that is its
// point instead, such as a GC stall or kill latency.
func scaled(d time.Duration) time.Duration {
	if n, err := strconv.Atoi(os.Getenv("PGMEM_TIME_SCALE")); err == nil && n > 1 {
		return d * time.Duration(n)
	}
	return d
}

// timeScaled reports whether PGMEM_TIME_SCALE is above 1. If it is, it
// logs that the latency bound named by what is skipped.
func timeScaled(t *testing.T, what string) bool {
	t.Helper()
	if scaled(1) > 1 {
		t.Logf("PGMEM_TIME_SCALE > 1: skipping the %s bound, the point of this test", what)
		return true
	}
	return false
}

// closeTimeout bounds Kernel.Close in tests. Every process must end well
// within it.
var closeTimeout = scaled(10 * time.Second)

// closeKernel closes k and fails the test if that takes longer than
// closeTimeout.
func closeKernel(t *testing.T, k *kernel.Kernel) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := k.Close(ctx); err != nil {
		t.Errorf("close kernel: %v", err)
	}
}

// checkFreed fails the test unless every memory k's allocator handed out
// has been freed, with its overlays, and every shared-memory segment too.
func checkFreed(t *testing.T, k *kernel.Kernel) {
	t.Helper()
	if st := kernel.MemoryStats(k); st.Live != 0 || st.Committed != 0 || st.Overlays != 0 {
		t.Errorf("memories not freed: %d live, %d bytes committed, %d overlays", st.Live, st.Committed, st.Overlays)
	}
	if segs, att := kernel.ShmStats(k); segs != 0 || att != 0 {
		t.Errorf("shared memory not freed: %d segments, %d attachments", segs, att)
	}
	if n := kernel.SemCount(k); n != 0 {
		t.Errorf("%d semaphores not freed", n)
	}
}

func run(t *testing.T, k *kernel.Kernel, s kernel.Spec) (string, string, kernel.Status) {
	t.Helper()
	var out, errb bytes.Buffer
	s.Stdout, s.Stderr = &out, &errb
	p, err := k.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	st := p.Wait()
	return out.String(), errb.String(), st
}

func TestProcsFixture(t *testing.T) {
	k := newKernel(t, "procs")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/procs"})
	if !st.Exited() || st.ExitCode() != 0 || out != "procs ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}

func TestStdioAndExitCodeFromGo(t *testing.T) {
	k := newKernel(t, "procs")
	out, _, st := run(t, k, kernel.Spec{Path: "/fixtures/procs", Args: []string{"upper"}, Stdin: strings.NewReader("abc")})
	if out != "ABC" || !st.Exited() || st.ExitCode() != 7 {
		t.Fatalf("%q status %#x", out, st)
	}
}

// TestExitCodesNotMistakenForKill checks that a process that exits 137 or
// 255 on its own, with its context alive, counts as exited, not killed.
// 137 is the host's kill code, 128+SIGKILL.
func TestExitCodesNotMistakenForKill(t *testing.T) {
	k := newKernel(t, "procs")
	for _, code := range []int{137, 255} {
		_, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/procs", Args: []string{fmt.Sprintf("exit%d", code)}})
		if st != kernel.ExitStatus(uint32(code)) || !st.Exited() || st.ExitCode() != code {
			t.Fatalf("exit(%d): status %#x\nstderr:\n%s", code, st, errOut)
		}
	}
}

func TestTrapStatusFromGo(t *testing.T) {
	k := newKernel(t, "procs")
	_, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/procs", Args: []string{"trap"}})
	if !st.Signaled() || st.Signal() != 6 {
		t.Fatalf("status %#x", st)
	}
	if errOut == "" {
		t.Fatal("trap not reported on stderr")
	}
}

func TestRegisterRefusesModuleWithoutABI(t *testing.T) {
	k := newKernel(t)
	wasm, err := os.ReadFile("testdata/noabi.wasm")
	if err != nil {
		t.Fatal(err)
	}
	m, err := k.Compile(context.Background(), wasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Register("/fixtures/noabi", m); err == nil || !strings.Contains(err.Error(), "pgmem_abi_version") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartUnknownPath(t *testing.T) {
	k := newKernel(t)
	if _, err := k.Start(context.Background(), kernel.Spec{Path: "/nope"}); err == nil {
		t.Fatal("started an unregistered path")
	}
}

func TestStatusEncoding(t *testing.T) {
	s := kernel.ExitStatus(3)
	if !s.Exited() || s.ExitCode() != 3 || s.Signaled() {
		t.Fatalf("%#x", s)
	}
	if !kernel.TrapStatus.Signaled() || kernel.TrapStatus.Signal() != 6 || kernel.TrapStatus.Exited() {
		t.Fatal("trap status")
	}
}

// readyWriter collects a process's output and closes ready at the first
// write.
type readyWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
}

func newReadyWriter() *readyWriter { return &readyWriter{ready: make(chan struct{})} }

func (w *readyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		close(w.ready)
	}
	return w.buf.Write(p)
}

func (w *readyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestCloseKillsBlockedProcesses closes the kernel while processes are
// blocked. One is in poll_oneoff, one in pgm.wait with a child blocked in
// poll, one in a pipe read, and one retries an interrupted sleep forever.
// Close must return well within closeTimeout. Each process must die of
// SIGKILL at its next host call rather than see EINTR and carry on.
func TestCloseKillsBlockedProcesses(t *testing.T) {
	for _, mode := range []string{"sleep", "waitchild", "piperead", "spin"} {
		t.Run(mode, func(t *testing.T) {
			k := newKernel(t, "procs")
			out := newReadyWriter()
			var errb bytes.Buffer
			p, err := k.Start(context.Background(), kernel.Spec{
				Path: "/fixtures/procs", Args: []string{mode}, Stdout: out, Stderr: &errb,
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-out.ready:
			case <-time.After(closeTimeout):
				t.Fatal("process never got ready")
			}
			time.Sleep(50 * time.Millisecond) // let it block
			start := time.Now()
			closeKernel(t, k)
			t.Logf("Close took %v", time.Since(start))
			done := make(chan kernel.Status, 1)
			go func() { done <- p.Wait() }()
			var st kernel.Status
			select {
			case st = <-done:
			case <-time.After(closeTimeout):
				t.Fatal("process still running after Close")
			}
			if st != kernel.KilledStatus || !st.Signaled() || st.Signal() != 9 {
				t.Fatalf("status %#x, want SIGKILL\nstdout:\n%s\nstderr:\n%s", st, out, errb.String())
			}
		})
	}
}

// TestArgvLimit checks that argv and environment may total 2 MiB, each
// string counted with its NUL. The bigargv child spawns at exactly the
// limit, then one byte over it, which fails with E2BIG. From Go, one byte
// over is an error too.
func TestArgvLimit(t *testing.T) {
	k := newKernel(t, "procs")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/procs", Args: []string{"bigargv"}})
	if !st.Exited() || st.ExitCode() != 0 {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%.200s", st, out, errOut)
	}
	const path = "/fixtures/procs"
	big := strings.Repeat("x", 2<<20-(len(path)+1)-(len("e=")+1))
	_, err := k.Start(context.Background(), kernel.Spec{Path: path, Args: []string{big}, Env: []string{"e="}})
	if !errors.Is(err, wasiabi.E2BIG) {
		t.Fatalf("Start with %d bytes of argv and env: %v", 2<<20+1, err)
	}
}

// failingAllocator hands out ok memories from a vmem allocator, then panics
// as vmem does when it cannot reserve address space.
type failingAllocator struct {
	ok   atomic.Int32
	vmem vmem.Allocator
}

func (a *failingAllocator) Allocate(cap, max uint64) experimental.LinearMemory {
	if a.ok.Add(-1) < 0 {
		panic("test: cannot reserve memory")
	}
	return a.vmem.Allocate(cap, max)
}

// TestInstantiatePanic checks that an instantiation that panics is an
// error from Start and ENOMEM from a guest's spawn. No process is left
// behind, so Close still finishes, and the spawn's dup'd fds are closed.
func TestInstantiatePanic(t *testing.T) {
	t.Run("Start", func(t *testing.T) {
		k := newKernel(t, "procs")
		kernel.SetMemoryAllocator(k, new(failingAllocator))
		_, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/procs"})
		if err == nil || !strings.Contains(err.Error(), "cannot reserve memory") {
			t.Fatalf("err = %v", err)
		}
		closeKernel(t, k)
	})
	t.Run("spawn", func(t *testing.T) {
		k := newKernel(t, "procs")
		a := new(failingAllocator)
		a.ok.Store(1) // the parent, not its child
		kernel.SetMemoryAllocator(k, a)
		out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/procs", Args: []string{"spawnfail"}})
		if !st.Exited() || st.ExitCode() != 0 {
			t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
		}
		closeKernel(t, k)
		if got := a.vmem.Stats(); got.Live != 0 {
			t.Fatalf("%d memories not freed", got.Live)
		}
	})
}

func TestStartAfterClose(t *testing.T) {
	k := newKernel(t, "procs")
	closeKernel(t, k)
	if _, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/procs"}); !errors.Is(err, kernel.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
