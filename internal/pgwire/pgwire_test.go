package pgwire

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func msg(typ byte, body ...byte) []byte {
	b := []byte{typ, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(4+len(body)))
	return append(b, body...)
}

// fakeServer reads the startup message and the query, replying with script
// after each, then expects Terminate.
func fakeServer(t *testing.T, c net.Conn, afterQuery ...[]byte) (query chan string) {
	query = make(chan string, 1)
	go func() {
		defer c.Close()
		var n [4]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(n[:]))-4)
		c.Write(append(msg('R', 0, 0, 0, 0), msg('Z', 'I')...))
		var h [5]byte
		if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 'Q' {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(h[1:])-4)
		io.ReadFull(c, body)
		query <- strings.TrimSuffix(string(body), "\x00")
		for _, m := range afterQuery {
			c.Write(m)
		}
		io.ReadFull(c, h[:]) // Terminate
	}()
	return query
}

func TestExecOK(t *testing.T) {
	cl, sv := net.Pipe()
	q := fakeServer(t, sv, msg('C', []byte("CHECKPOINT\x00")...), msg('Z', 'I'))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Exec(ctx, cl, "postgres", "postgres", "CHECKPOINT"); err != nil {
		t.Fatal(err)
	}
	if got := <-q; got != "CHECKPOINT" {
		t.Fatalf("query %q", got)
	}
}

func TestExecError(t *testing.T) {
	cl, sv := net.Pipe()
	fields := []byte("SERROR\x00C42601\x00Msyntax error\x00\x00")
	fakeServer(t, sv, msg('N', 0), msg('E', fields...), msg('Z', 'I'))
	err := Exec(context.Background(), cl, "postgres", "postgres", "bogus")
	if err == nil || !strings.Contains(err.Error(), "42601") || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecAuthRequest(t *testing.T) {
	cl, sv := net.Pipe()
	go func() {
		defer sv.Close()
		var n [4]byte
		io.ReadFull(sv, n[:])
		io.CopyN(io.Discard, sv, int64(binary.BigEndian.Uint32(n[:]))-4)
		sv.Write(msg('R', 0, 0, 0, 3))
	}()
	if err := Exec(context.Background(), cl, "postgres", "postgres", "x"); err == nil {
		t.Fatal("want error")
	}
}

// readStartup reads the startup message from the client side of c.
func readStartup(c net.Conn) {
	var n [4]byte
	io.ReadFull(c, n[:])
	io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(n[:]))-4)
}

// A FATAL at startup comes back with its SQLSTATE and message, not just
// EOF. The server sends ErrorResponse, then closes.
func TestExecStartupFatal(t *testing.T) {
	cl, sv := net.Pipe()
	go func() {
		defer sv.Close()
		readStartup(sv)
		sv.Write(msg('E', []byte("SFATAL\x00C3D000\x00Mdatabase \"nope\" does not exist\x00\x00")...))
	}()
	err := Exec(context.Background(), cl, "postgres", "nope", "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "3D000") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want the FATAL's SQLSTATE and message", err)
	}
}

// A message length outside 4..16 MiB is a protocol error, and Exec reads
// no further.
func TestExecBadLength(t *testing.T) {
	for _, n := range []uint32{3, 1<<24 + 1} {
		cl, sv := net.Pipe()
		go func() {
			defer sv.Close()
			readStartup(sv)
			h := []byte{'R', 0, 0, 0, 0}
			binary.BigEndian.PutUint32(h[1:], n)
			sv.Write(h)
		}()
		err := Exec(context.Background(), cl, "postgres", "postgres", "x")
		if err == nil || !strings.Contains(err.Error(), "bad message length") {
			t.Errorf("length %d: err = %v, want bad message length", n, err)
		}
	}
}

// A done ctx closes the connection, which ends a wait for a server that
// never answers. The error wraps ctx's.
func TestExecContextCancel(t *testing.T) {
	cl, sv := net.Pipe()
	defer sv.Close()
	go func() {
		readStartup(sv)
		io.Copy(io.Discard, sv) // never answers
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Exec(ctx, cl, "postgres", "postgres", "x") }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec still waiting 5 s after its context ended")
	}
}
