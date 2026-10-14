package wasi_test

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

	"golang.org/x/net/nettest"

	"github.com/feverenergy/pgmem/internal/wasi"
)

// TestConnConformance runs x/net's net.Conn conformance suite over a
// socket pair.
func TestConnConformance(t *testing.T) {
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		a, b := wasi.NewSockPair()
		c1, c2 = wasi.NewConn(a), wasi.NewConn(b)
		return c1, c2, func() { c1.Close(); c2.Close() }, nil
	})
}

// These are the contract points pgx relies on that nettest does not pin
// down: bare io.EOF, EPIPE as a *net.OpError, Close idempotence and Unix
// addresses.
func TestConnContract(t *testing.T) {
	l := listener(t, "/tmp/l.sock", 1)
	s := wasi.NewSock()
	mustOK(t, s.Connect(l))
	srv, errno := l.Accept()
	mustOK(t, errno)
	c, p := wasi.NewConn(s), wasi.NewConn(srv)
	defer c.Close()
	if a, ok := c.RemoteAddr().(*net.UnixAddr); !ok || a.Name != "/tmp/l.sock" || a.Net != "unix" {
		t.Fatalf("remote addr %#v", c.RemoteAddr())
	}
	if a, ok := p.LocalAddr().(*net.UnixAddr); !ok || a.Name != "/tmp/l.sock" {
		t.Fatalf("server local addr %#v", p.LocalAddr())
	}
	if a, ok := c.LocalAddr().(*net.UnixAddr); !ok || a.Name != "" {
		t.Fatalf("client local addr %#v", c.LocalAddr())
	}

	if _, err := p.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := p.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second close: %v", err)
	}
	if _, err := p.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
	b := make([]byte, 2)
	if n, err := c.Read(b); n != 1 || err != nil {
		t.Fatalf("read buffered: %d %v", n, err)
	}
	if n, err := c.Read(b); n != 0 || err != io.EOF {
		t.Fatalf("read at EOF: %d %v (want bare io.EOF)", n, err)
	}
	_, err := c.Write([]byte("y"))
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "write" || oe.Net != "unix" || !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("write to closed peer: %#v", err)
	}
}
