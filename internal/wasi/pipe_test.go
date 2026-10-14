package wasi_test

import (
	"sync"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasi/wasitest"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

func TestPollAndNonblockingPipe(t *testing.T) {
	r, w := wasi.NewPipe()
	res := wasitest.Run(t, fixture(t, "pollpipe.wasm"), wasitest.Options{Extra: map[int32]wasi.File{4: r, 5: w}}, "/pollpipe")
	if res.ExitCode != 0 || res.Stdout != "poll ok\n" {
		t.Fatalf("exit %d trapped %v\n%s%s", res.ExitCode, res.Trapped, res.Stdout, res.Stderr)
	}
}

// The Go side writes more than the pipe holds, slowly, then closes. The
// guest must block, wake and see every byte and EOF.
func TestPipeBackpressure(t *testing.T) {
	r, w := wasi.NewPipe()
	const total = 3*wasi.PipeCapacity + 123
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunk := make([]byte, 10_000)
		for sent := 0; sent < total; {
			k := min(len(chunk), total-sent)
			got, err := w.Write(chunk[:k], 0)
			if err == wasiabi.EAGAIN {
				time.Sleep(time.Millisecond) // full, so the guest drains it
				continue
			}
			if err != 0 || got == 0 {
				t.Errorf("write: %d %d", got, err)
				return
			}
			sent += got
			time.Sleep(time.Millisecond)
		}
		w.Close()
	}()
	res := wasitest.Run(t, fixture(t, "pipewait.wasm"), wasitest.Options{Extra: map[int32]wasi.File{4: r}}, "/pipewait")
	wg.Wait()
	if res.Stdout != "196731 eof\n" {
		t.Fatalf("%q %q", res.Stdout, res.Stderr)
	}
}

func TestWriteAfterReaderClosed(t *testing.T) {
	r, w := wasi.NewPipe()
	r.Close()
	if _, err := w.Write([]byte("x"), 0); err != wasiabi.EPIPE {
		t.Fatalf("errno %d, want EPIPE", err)
	}
}

// A pipe's state change wakes waiters on that pipe only.
func TestPerFileWakeups(t *testing.T) {
	ra, wa := wasi.NewPipe()
	rb, wb := wasi.NewPipe()
	defer ra.Close()
	defer wa.Close()
	defer rb.Close()
	defer wb.Close()
	cha := ra.(wasi.Waitable).Changed()
	if _, err := wb.Write([]byte("x"), 0); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	select {
	case <-cha:
		t.Fatal("a write to pipe B woke a waiter on pipe A")
	default:
	}
	if _, err := wa.Write([]byte("y"), 0); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	select {
	case <-cha:
	default:
		t.Fatal("a write to pipe A did not wake its waiter")
	}
	// Both ends share the queue, so the writer's waiters see the reader
	// drain.
	chw := wa.(wasi.Waitable).Changed()
	if _, err := ra.Read(make([]byte, 1), 0); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	select {
	case <-chw:
	default:
		t.Fatal("a read did not wake the writer's waiter")
	}
}

// Pipe ends never block on their own now. Blocking is the WASI call's
// job.
func TestPipeEndsNeverBlock(t *testing.T) {
	r, w := wasi.NewPipe()
	if _, err := r.Read(make([]byte, 1), 0); err != wasiabi.EAGAIN {
		t.Fatalf("empty read: errno %d, want EAGAIN", err)
	}
	big := make([]byte, wasi.PipeCapacity)
	if n, err := w.Write(big, 0); err != 0 || n != wasi.PipeCapacity {
		t.Fatalf("fill: %d %d", n, err)
	}
	if _, err := w.Write([]byte("x"), 0); err != wasiabi.EAGAIN {
		t.Fatalf("full write: errno %d, want EAGAIN", err)
	}
}

// Many writes interleave with blocking reads through the guest. No lost
// wake-up may stall the reader.
func TestBlockingReadsNeverMissWrites(t *testing.T) {
	r, w := wasi.NewPipe()
	const total = 5000
	go func() {
		for i := 0; i < total; i++ {
			for {
				if _, err := w.Write([]byte{1}, 0); err == 0 {
					break
				}
				time.Sleep(time.Microsecond)
			}
		}
		w.Close()
	}()
	res := wasitest.Run(t, fixture(t, "pipewait.wasm"), wasitest.Options{Extra: map[int32]wasi.File{4: r}}, "/pipewait")
	if res.Stdout != "5000 eof\n" {
		t.Fatalf("%q %q", res.Stdout, res.Stderr)
	}
}

// blockedRead runs pipewait on r. Once the guest has drained a first
// write, it is blocked in fd_read or about to be, and blockedRead runs
// wake. It returns what the guest printed.
func blockedRead(t *testing.T, wake func(r, w wasi.File)) string {
	t.Helper()
	r, w := wasi.NewPipe()
	defer w.Close()
	go func() {
		w.Write([]byte("abc"), 0)
		deadline := time.Now().Add(30 * time.Second)
		for {
			if ready, _, _ := r.Ready(wasiabi.EventtypeFdRead); !ready || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		wake(r, w)
	}()
	return wasitest.Run(t, fixture(t, "pipewait.wasm"), wasitest.Options{Extra: map[int32]wasi.File{4: r}}, "/pipewait").Stdout
}

// Closing the read end while the guest is blocked reading it wakes the
// read with EBADF. This is an fd_close racing the call on another
// thread.
func TestCloseWakesBlockedCallOnSameEnd(t *testing.T) {
	if out := blockedRead(t, func(r, _ wasi.File) { r.Close() }); out != "3 error\n" {
		t.Fatalf("guest printed %q, want a read error after 3 bytes", out)
	}
}

// Closing the write end wakes a guest blocked reading with EOF.
func TestCloseWakesBlockedPeer(t *testing.T) {
	if out := blockedRead(t, func(_, w wasi.File) { w.Close() }); out != "3 eof\n" {
		t.Fatalf("guest printed %q, want EOF after 3 bytes", out)
	}
}

// A second close of an end succeeds. Writing to a closed write end fails
// with EBADF.
func TestClosedWriteEnd(t *testing.T) {
	r, w := wasi.NewPipe()
	defer r.Close()
	w.Close()
	if err := w.Close(); err != 0 {
		t.Fatalf("second close: %d", err)
	}
	if _, err := w.Write([]byte("x"), 0); err != wasiabi.EBADF {
		t.Fatalf("write after close: %d", err)
	}
}

// A non-blocking write of at most PIPE_BUF bytes is all or nothing. A
// larger one writes what fits.
func TestNonblockingWrite(t *testing.T) {
	r, w := wasi.NewPipe()
	defer r.Close()
	defer w.Close()
	nb := wasiabi.FdflagNonblock
	if n, err := w.Write(make([]byte, wasi.PipeCapacity-100), nb); n != wasi.PipeCapacity-100 || err != 0 {
		t.Fatalf("fill: %d %d", n, err)
	}
	if ready, room, _ := w.Ready(wasiabi.EventtypeFdWrite); ready || room != 100 {
		t.Fatalf("Ready(write) = %v, %d with 100 bytes free", ready, room)
	}
	if n, err := w.Write(make([]byte, 200), nb); n != 0 || err != wasiabi.EAGAIN {
		t.Fatalf("small write: %d %d, want EAGAIN", n, err)
	}
	if n, err := w.Write(make([]byte, 5000), nb); n != 100 || err != 0 {
		t.Fatalf("large write: %d %d, want 100", n, err)
	}
	if n, err := w.Write([]byte("x"), nb); n != 0 || err != wasiabi.EAGAIN {
		t.Fatalf("write to a full pipe: %d %d", n, err)
	}
	if ready, n, hup := r.Ready(wasiabi.EventtypeFdRead); !ready || n != wasi.PipeCapacity || hup {
		t.Fatalf("Ready(read) = %v %d %v", ready, n, hup)
	}
}
