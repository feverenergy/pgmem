package wasi_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

func mustOK(t *testing.T, err wasiabi.Errno) {
	t.Helper()
	if err != wasiabi.ESUCCESS {
		t.Fatalf("errno %d", err)
	}
}

func listener(t *testing.T, path string, backlog int) *wasi.Sock {
	t.Helper()
	l := wasi.NewSock()
	mustOK(t, l.Bind(path, nil))
	mustOK(t, l.Listen(backlog))
	t.Cleanup(func() { l.Close() })
	return l
}

func TestSockConnectAccept(t *testing.T) {
	l := listener(t, "/tmp/.s.test", 2)
	if _, err := l.Accept(); err != wasiabi.EAGAIN {
		t.Fatalf("accept on empty queue: %d", err)
	}
	ch := l.Changed()
	c := wasi.NewSock()
	defer c.Close()
	mustOK(t, c.Connect(l))
	select {
	case <-ch:
	default:
		t.Fatal("connect did not wake the listener's waiter")
	}
	if ok, _, _ := l.Ready(wasiabi.EventtypeFdRead); !ok {
		t.Fatal("listener with a pending connection not readable")
	}
	// Data sent before accept arrives.
	if n, err := c.Write([]byte("hello"), 0); n != 5 || err != wasiabi.ESUCCESS {
		t.Fatalf("write %d %d", n, err)
	}
	s, err := l.Accept()
	mustOK(t, err)
	defer s.Close()
	if s.Addr() != "/tmp/.s.test" {
		t.Fatalf("server addr %q", s.Addr())
	}
	if p, err := s.PeerAddr(); err != wasiabi.ESUCCESS || p != "" {
		t.Fatalf("server peer %q %d", p, err)
	}
	if p, err := c.PeerAddr(); err != wasiabi.ESUCCESS || p != "/tmp/.s.test" {
		t.Fatalf("client peer %q %d", p, err)
	}
	buf := make([]byte, 16)
	if n, err := s.Read(buf, 0); err != wasiabi.ESUCCESS || string(buf[:n]) != "hello" {
		t.Fatalf("read %q %d", buf[:n], err)
	}
	if _, err := s.Read(buf, 0); err != wasiabi.EAGAIN {
		t.Fatalf("empty read: %d", err)
	}
	if s.Filetype() != wasiabi.FiletypeSocketStream {
		t.Fatalf("filetype %d", s.Filetype())
	}
}

func TestSockBacklogAndRefusal(t *testing.T) {
	l := listener(t, "/tmp/.s.b", 1)
	c1, c2 := wasi.NewSock(), wasi.NewSock()
	defer c1.Close()
	defer c2.Close()
	mustOK(t, c1.Connect(l))
	if err := c2.Connect(l); err != wasiabi.EAGAIN {
		t.Fatalf("connect to full queue: %d", err)
	}
	if err := c1.Connect(l); err != wasiabi.EISCONN {
		t.Fatalf("second connect: %d", err)
	}
	notListening := wasi.NewSock()
	mustOK(t, notListening.Bind("/tmp/.s.n", nil))
	if err := c2.Connect(notListening); err != wasiabi.ECONNREFUSED {
		t.Fatalf("connect to bound, not listening: %d", err)
	}
	// Closing the listener drops the queued connection, so its client reads
	// EOF.
	l.Close()
	if n, err := c1.Read(make([]byte, 4), 0); n != 0 || err != wasiabi.ESUCCESS {
		t.Fatalf("read after listener closed: %d %d", n, err)
	}
	if err := c2.Connect(l); err != wasiabi.ECONNREFUSED {
		t.Fatalf("connect to closed listener: %d", err)
	}
}

func TestSockShutdown(t *testing.T) {
	a, b := wasi.NewSockPair()
	defer a.Close()
	defer b.Close()
	mustOK(t, a.Shutdown(wasiabi.SdflagsWr))
	if n, err := b.Read(make([]byte, 4), 0); n != 0 || err != wasiabi.ESUCCESS {
		t.Fatalf("peer read after SHUT_WR: %d %d", n, err)
	}
	if ok, _, hup := b.Ready(wasiabi.EventtypeFdRead); !ok || !hup {
		t.Fatalf("peer readiness after SHUT_WR: %v %v", ok, hup)
	}
	if _, err := a.Write([]byte("x"), 0); err != wasiabi.EPIPE {
		t.Fatalf("write after own SHUT_WR: %d", err)
	}
	// The other direction still works after a half-close.
	if n, err := b.Write([]byte("late"), 0); n != 4 || err != wasiabi.ESUCCESS {
		t.Fatalf("write back: %d %d", n, err)
	}
	buf := make([]byte, 8)
	if n, err := a.Read(buf, 0); err != wasiabi.ESUCCESS || string(buf[:n]) != "late" {
		t.Fatalf("read back %q %d", buf[:n], err)
	}
	if err := a.Shutdown(9); err != wasiabi.EINVAL {
		t.Fatalf("bad how: %d", err)
	}
	if err := wasi.NewSock().Shutdown(wasiabi.SdflagsRd); err != wasiabi.ENOTCONN {
		t.Fatalf("shutdown unconnected: %d", err)
	}
}

func TestSockPairClose(t *testing.T) {
	a, b := wasi.NewSockPair()
	ch := b.Changed()
	a.Close()
	select {
	case <-ch:
	default:
		t.Fatal("close did not wake the peer's waiter")
	}
	if n, err := b.Read(make([]byte, 4), 0); n != 0 || err != wasiabi.ESUCCESS {
		t.Fatalf("read from closed peer: %d %d", n, err)
	}
	if _, err := b.Write([]byte("x"), 0); err != wasiabi.EPIPE {
		t.Fatalf("write to closed peer: %d", err)
	}
	if _, err := a.Read(make([]byte, 4), 0); err != wasiabi.EBADF {
		t.Fatalf("read on closed socket: %d", err)
	}
	b.Close()
	b.Close() // idempotent
}

func TestSockBackpressure(t *testing.T) {
	a, b := wasi.NewSockPair()
	defer a.Close()
	defer b.Close()
	chunk := bytes.Repeat([]byte("z"), 64<<10)
	total := 0
	for {
		n, err := a.Write(chunk, 0)
		total += n
		if err == wasiabi.EAGAIN {
			break
		}
		mustOK(t, err)
	}
	if total != wasi.SockCapacity {
		t.Fatalf("buffered %d, want %d", total, wasi.SockCapacity)
	}
	if ok, _, _ := a.Ready(wasiabi.EventtypeFdWrite); ok {
		t.Fatal("full socket reported writable")
	}
	ch := a.Changed()
	if _, err := b.Read(make([]byte, 8192), 0); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("drain did not wake the writer")
	}
}

func TestSockUnboundAndListenErrors(t *testing.T) {
	s := wasi.NewSock()
	defer s.Close()
	if _, err := s.Read(make([]byte, 1), 0); err != wasiabi.ENOTCONN {
		t.Fatalf("read unbound: %d", err)
	}
	if err := s.Listen(5); err != wasiabi.EINVAL {
		t.Fatalf("listen unbound: %d", err)
	}
	if _, err := s.Accept(); err != wasiabi.EINVAL {
		t.Fatalf("accept unbound: %d", err)
	}
	mustOK(t, s.Bind("/tmp/.s.u", nil))
	if err := s.Bind("/tmp/.s.v", nil); err != wasiabi.EINVAL {
		t.Fatalf("second bind: %d", err)
	}
	closed := 0
	l := wasi.NewSock()
	mustOK(t, l.Bind("/tmp/.s.w", func() { closed++ }))
	l.Close()
	l.Close()
	if closed != 1 {
		t.Fatalf("onClose ran %d times", closed)
	}
}

// A local SHUT_RD reads EOF at once and reports readable, even with data
// buffered. The peer's writes fail with EPIPE. A local SHUT_WR reports
// writable, and writing fails with EPIPE.
func TestSockLocalShutdownReadiness(t *testing.T) {
	a, b := wasi.NewSockPair()
	defer a.Close()
	defer b.Close()
	if n, err := b.Write([]byte("queued"), 0); n != 6 || err != wasiabi.ESUCCESS {
		t.Fatalf("write %d %d", n, err)
	}
	ch := b.Changed()
	mustOK(t, a.Shutdown(wasiabi.SdflagsRd))
	select {
	case <-ch:
	default:
		t.Fatal("SHUT_RD did not wake the peer")
	}
	if n, err := a.Read(make([]byte, 8), 0); n != 0 || err != wasiabi.ESUCCESS {
		t.Fatalf("read after own SHUT_RD: %d %d", n, err)
	}
	if ok, _, _ := a.Ready(wasiabi.EventtypeFdRead); !ok {
		t.Fatal("read-shut socket not readable")
	}
	if _, err := b.Write([]byte("x"), 0); err != wasiabi.EPIPE {
		t.Fatalf("peer write after SHUT_RD: %d", err)
	}
	if ok, _, hup := b.Ready(wasiabi.EventtypeFdWrite); !ok || !hup {
		t.Fatalf("peer write readiness after SHUT_RD: %v %v", ok, hup)
	}

	// Fill a's outgoing buffer, then shut it. It is writable again, and
	// writes fail with EPIPE.
	for {
		if _, err := a.Write(make([]byte, 64<<10), 0); err == wasiabi.EAGAIN {
			break
		}
	}
	if ok, _, _ := a.Ready(wasiabi.EventtypeFdWrite); ok {
		t.Fatal("full socket reported writable")
	}
	mustOK(t, a.Shutdown(wasiabi.SdflagsWr))
	if ok, _, _ := a.Ready(wasiabi.EventtypeFdWrite); !ok {
		t.Fatal("write-shut socket not writable")
	}
	if _, err := a.Write([]byte("x"), 0); err != wasiabi.EPIPE {
		t.Fatalf("write after own SHUT_WR: %d", err)
	}
	// What a sent before SHUT_WR still arrives, then EOF.
	got := 0
	buf := make([]byte, 64<<10)
	for {
		n, err := b.Read(buf, 0)
		mustOK(t, err)
		if n == 0 {
			break
		}
		got += n
	}
	if got != wasi.SockCapacity {
		t.Fatalf("peer read %d bytes before EOF, want %d", got, wasi.SockCapacity)
	}
	mustOK(t, a.Shutdown(wasiabi.SdflagsRd|wasiabi.SdflagsWr)) // again, a no-op
}

// Each state change wakes the waiters on the socket's previous queue.
func TestSockStateChangesWakeOldQueue(t *testing.T) {
	l := wasi.NewSock()
	mustOK(t, l.Bind("/tmp/.s.q", nil))
	ch := l.Changed()
	mustOK(t, l.Listen(0)) // clamped to 1
	select {
	case <-ch:
	default:
		t.Fatal("listen did not wake the old queue")
	}
	if !l.Listening() {
		t.Fatal("not listening")
	}
	mustOK(t, l.Listen(1)) // again, which changes the backlog only
	if err := l.Connect(l); err != wasiabi.EINVAL {
		t.Fatalf("connect from a listener: %d", err)
	}

	c := wasi.NewSock()
	ch = c.Changed()
	if ok, _, _ := c.Ready(wasiabi.EventtypeFdWrite); ok {
		t.Fatal("unbound socket writable")
	}
	mustOK(t, c.Connect(l))
	select {
	case <-ch:
	default:
		t.Fatal("connect did not wake the client's old queue")
	}
	if err := c.Listen(1); err != wasiabi.EINVAL {
		t.Fatalf("listen on a connected socket: %d", err)
	}
	if err := c.Bind("/tmp/.s.c", nil); err != wasiabi.EINVAL {
		t.Fatalf("bind a connected socket: %d", err)
	}
	if ok, _, _ := c.Ready(wasiabi.EventtypeFdWrite); !ok {
		t.Fatal("connected socket not writable")
	}

	ch = l.Changed()
	s, err := l.Accept()
	mustOK(t, err)
	select {
	case <-ch:
	default:
		t.Fatal("accept did not wake a waiter for room in the queue")
	}
	if s.Listening() {
		t.Fatal("accepted socket listening")
	}

	ch = s.Changed()
	s.Close()
	select {
	case <-ch:
	default:
		t.Fatal("close did not wake the pair's queue")
	}
	if ok, _, _ := s.Ready(wasiabi.EventtypeFdWrite); !ok {
		t.Fatal("closed socket not ready")
	}
	if _, err := s.Write([]byte("x"), 0); err != wasiabi.EBADF {
		t.Fatalf("write on closed: %d", err)
	}
	if err := s.Shutdown(wasiabi.SdflagsRd); err != wasiabi.EBADF {
		t.Fatalf("shutdown closed: %d", err)
	}
	if _, err := s.PeerAddr(); err != wasiabi.EBADF {
		t.Fatalf("peer addr of closed: %d", err)
	}
	if _, err := c.Write([]byte("x"), 0); err != wasiabi.EPIPE {
		t.Fatalf("write to a closed peer: %d", err)
	}

	ch = l.Changed()
	l.Close()
	select {
	case <-ch:
	default:
		t.Fatal("close did not wake the listener's queue")
	}
	if _, err := l.Accept(); err != wasiabi.EBADF {
		t.Fatalf("accept on closed: %d", err)
	}
	if ok, _, _ := l.Ready(wasiabi.EventtypeFdRead); !ok {
		t.Fatal("closed listener not ready")
	}
	c.Close()
}

// Two bound sockets connecting to each other at once must not deadlock.
// Neither is listening, so both are refused.
func TestSockMutualConnectRefused(t *testing.T) {
	done := make(chan string, 1)
	go func() {
		for i := 0; i < 1000; i++ {
			a, b := wasi.NewSock(), wasi.NewSock()
			a.Bind("/tmp/.s.a", nil)
			b.Bind("/tmp/.s.b", nil)
			errs := make(chan wasiabi.Errno, 2)
			go func() { errs <- a.Connect(b) }()
			go func() { errs <- b.Connect(a) }()
			for range 2 {
				if err := <-errs; err != wasiabi.ECONNREFUSED {
					done <- "mutual connect: errno " + err.Error()
					return
				}
			}
			a.Close()
			b.Close()
		}
		done <- ""
	}()
	select {
	case msg := <-done:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: mutual connects never returned")
	}
}
