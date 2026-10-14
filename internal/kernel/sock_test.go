package kernel_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/wasi"
)

// mkdirTmp creates /tmp in k's file system.
func mkdirTmp(t *testing.T, k *kernel.Kernel) {
	t.Helper()
	if err := kernel.FS(k).Mkdir(kernel.FS(k).Root(), "tmp"); err != 0 {
		t.Fatalf("mkdir /tmp: %v", err)
	}
}

func TestSockFixture(t *testing.T) {
	k := newKernel(t, "sock")
	mkdirTmp(t, k)
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/sock"})
	if !st.Exited() || st.ExitCode() != 0 || out != "sock ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}

// startEcho starts the fixture's echo server at path and waits until it
// listens. Later output, such as a failing child's message, goes to the
// test log, so a child that fails exits instead of blocking on a full
// stdout.
func startEcho(t *testing.T, k *kernel.Kernel, path string) {
	t.Helper()
	r, w := io.Pipe()
	var out []byte
	var done chan struct{} // set once the output is being read
	t.Cleanup(func() {
		w.Close()
		if done != nil {
			<-done // ReadAll returns at the close
			if len(out) > 0 {
				t.Logf("echo output: %s", out)
			}
		}
	})
	if _, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/sock", Args: []string{"echo", path}, Stdout: w}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 'r' {
		t.Fatalf("echo ready: %q %v", b, err)
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		out, _ = io.ReadAll(r)
	}()
}

// Twenty Go clients dial a guest listener at once. Each connection gets
// its own guest process, as the postmaster hands clients to backends.
func TestDialEcho(t *testing.T) {
	k := newKernel(t, "sock")
	mkdirTmp(t, k)
	startEcho(t, k, "/tmp/echo.sock")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := k.Dial(context.Background(), "/tmp/echo.sock")
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			if ra, ok := c.RemoteAddr().(*net.UnixAddr); !ok || ra.Name != "/tmp/echo.sock" {
				t.Errorf("remote addr %v", c.RemoteAddr())
			}
			for j := range 100 {
				msg := fmt.Sprintf("client %d message %d", i, j)
				if _, err := c.Write([]byte(msg)); err != nil {
					t.Error(err)
					return
				}
				buf := make([]byte, len(msg))
				if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
					t.Errorf("echo %q %v", buf, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestDialErrors(t *testing.T) {
	k := newKernel(t, "sock")
	mkdirTmp(t, k)
	if _, err := k.Dial(context.Background(), "/tmp/none.sock"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("dial missing path: %v", err)
	}
	if err := kernel.FS(k).WriteFile("/tmp/plain", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Dial(context.Background(), "/tmp/plain"); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial plain file: %v", err)
	}
	if _, err := k.Dial(context.Background(), "relative.sock"); err == nil {
		t.Fatal("dial relative path succeeded")
	}
}

// Closing the kernel ends the guest side, so a pending Read returns
// promptly.
func TestDialAfterKernelClose(t *testing.T) {
	k := newKernel(t, "sock")
	mkdirTmp(t, k)
	startEcho(t, k, "/tmp/echo.sock")
	c, err := k.Dial(context.Background(), "/tmp/echo.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		done <- err
	}()
	closeKernel(t, k)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read returned data after kernel close")
		}
	case <-time.After(scaled(5 * time.Second)):
		t.Fatal("read still blocked after kernel close")
	}
	if _, err := k.Dial(context.Background(), "/tmp/echo.sock"); err == nil {
		t.Fatal("dial after close succeeded")
	}
}

// listen makes a Go-side listener at path in k with backlog. The test's
// cleanup closes it.
func listen(t *testing.T, k *kernel.Kernel, path string, backlog int) *wasi.Sock {
	t.Helper()
	l, errno := kernel.Listen(k, path, backlog)
	if errno != 0 {
		t.Fatalf("listen %s: errno %d", path, errno)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// Dial with a context that is already done fails at once, even when the
// listener has room.
func TestDialContextDone(t *testing.T) {
	k := newKernel(t)
	mkdirTmp(t, k)
	listen(t, k, "/tmp/l.sock", 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := k.Dial(ctx, "/tmp/l.sock")
	if !errors.Is(err, context.Canceled) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("dial with a done context: %v, want context.Canceled", err)
	}
}

// While the listener's queue is full, Dial waits. An accept lets it
// through, and the end of its context ends it.
func TestDialWaitsForBacklog(t *testing.T) {
	k := newKernel(t)
	mkdirTmp(t, k)
	l := listen(t, k, "/tmp/l.sock", 1)
	first, err := k.Dial(context.Background(), "/tmp/l.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	type result struct {
		c   net.Conn
		err error
	}
	dial := func(ctx context.Context) chan result {
		ch := make(chan result, 1)
		go func() {
			c, err := k.Dial(ctx, "/tmp/l.sock")
			ch <- result{c, err}
		}()
		return ch
	}
	// notYet fails the test if ch delivers within a short while. The wait
	// only gives a wrong Dial time to return. A right one passes either
	// way.
	notYet := func(ch chan result) {
		t.Helper()
		select {
		case r := <-ch:
			t.Fatalf("dial returned while the queue was full: %v %v", r.c, r.err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	await := func(ch chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(scaled(5 * time.Second)):
			t.Fatal("dial still blocked")
		}
		panic("unreachable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := dial(ctx)
	notYet(waiting)
	cancel()
	if r := await(waiting); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("dial after cancel: %v %v, want context.Canceled", r.c, r.err)
	}

	waiting = dial(context.Background())
	notYet(waiting)
	s, errno := l.Accept()
	if errno != 0 {
		t.Fatalf("accept: errno %d", errno)
	}
	defer s.Close()
	r := await(waiting)
	if r.err != nil {
		t.Fatalf("dial after accept: %v", r.err)
	}
	r.c.Close()
}
