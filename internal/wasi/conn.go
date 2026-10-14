package wasi

import (
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// NewConn returns a net.Conn over the connected socket s, for Go code. Its
// Read and Write block, waiting on the socket's queue, a deadline or Close.
// Closing the conn closes s.
func NewConn(s *Sock) net.Conn {
	peer, _ := s.PeerAddr() // "" when s is not connected
	return &conn{
		s:      s,
		local:  net.UnixAddr{Name: s.Addr(), Net: "unix"},
		remote: net.UnixAddr{Name: peer, Net: "unix"},
		wlock:  make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

// conn is a net.Conn over a connected Sock, for Go code. Its calls block,
// waiting on the socket's queue, a deadline, or Close.
type conn struct {
	s             *Sock
	local, remote net.UnixAddr
	rd, wd        connDeadline  // read and write deadlines
	wlock         chan struct{} // held by a Write, so concurrent Writes do not interleave
	closeOnce     sync.Once
	closed        chan struct{}
}

// Read blocks until data arrives, EOF (io.EOF), the read deadline, or
// Close. A past deadline fails even with data buffered, as with Go's own
// conns. A zero-length read returns 0, nil.
func (c *conn) Read(b []byte) (int, error) {
	for {
		if err := c.check("read", &c.rd); err != nil {
			return 0, err
		}
		changed := c.s.Changed()
		n, err := c.s.Read(b, 0)
		switch {
		case n > 0 || (err == wasiabi.ESUCCESS && len(b) == 0):
			return n, nil
		case err == wasiabi.ESUCCESS:
			return 0, io.EOF
		case err == wasiabi.EBADF:
			return 0, c.opErr("read", net.ErrClosed)
		case err != wasiabi.EAGAIN:
			return 0, c.opErr("read", SyscallErrno(err))
		}
		select {
		case <-changed:
		case <-c.rd.wait():
			return 0, c.opErr("read", os.ErrDeadlineExceeded)
		case <-c.closed:
			return 0, c.opErr("read", net.ErrClosed)
		}
	}
}

// Write writes all of b, blocking while the buffer is full. It ends early
// at the write deadline, Close, or when the peer is gone (EPIPE).
// Concurrent Writes are serialized.
func (c *conn) Write(b []byte) (int, error) {
	select {
	case c.wlock <- struct{}{}:
		defer func() { <-c.wlock }()
	case <-c.closed:
		return 0, c.opErr("write", net.ErrClosed)
	}
	n := 0
	for {
		if err := c.check("write", &c.wd); err != nil {
			return n, err
		}
		changed := c.s.Changed()
		m, err := c.s.Write(b[n:], 0)
		n += m
		switch {
		case n == len(b) && err == wasiabi.ESUCCESS:
			return n, nil
		case err == wasiabi.ESUCCESS:
			continue // partial, so try the rest at once
		case err == wasiabi.EBADF:
			return n, c.opErr("write", net.ErrClosed)
		case err != wasiabi.EAGAIN:
			return n, c.opErr("write", SyscallErrno(err))
		}
		select {
		case <-changed:
		case <-c.wd.wait():
			return n, c.opErr("write", os.ErrDeadlineExceeded)
		case <-c.closed:
			return n, c.opErr("write", net.ErrClosed)
		}
	}
}

// check fails a call about to try op once the conn is closed or d passed.
func (c *conn) check(op string, d *connDeadline) error {
	select {
	case <-c.closed:
		return c.opErr(op, net.ErrClosed)
	default:
	}
	select {
	case <-d.wait():
		return c.opErr(op, os.ErrDeadlineExceeded)
	default:
	}
	return nil
}

// Close closes the socket, so the peer reads EOF, and makes pending calls
// return net.ErrClosed. Only the first call returns nil.
func (c *conn) Close() error {
	err := c.opErr("close", net.ErrClosed)
	c.closeOnce.Do(func() {
		close(c.closed)
		c.s.Close()
		c.rd.stop()
		c.wd.stop()
		err = nil
	})
	return err
}

func (c *conn) LocalAddr() net.Addr  { a := c.local; return &a }
func (c *conn) RemoteAddr() net.Addr { a := c.remote; return &a }

// SetDeadline, SetReadDeadline and SetWriteDeadline fail with
// net.ErrClosed once the conn is closed, and then arm no timer.
func (c *conn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *conn) SetReadDeadline(t time.Time) error  { return c.setDeadline(&c.rd, t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.setDeadline(&c.wd, t) }

func (c *conn) setDeadline(d *connDeadline, t time.Time) error {
	if !d.set(t, c.closed) {
		return c.opErr("set", net.ErrClosed)
	}
	return nil
}

func (c *conn) opErr(op string, err error) error {
	return &net.OpError{Op: op, Net: "unix", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: err}
}

// SyscallErrno maps a wasiabi errno that a socket or path lookup can
// return onto the host's syscall.Errno, so Go callers can match it with
// errors.Is. Any other errno becomes EIO.
func SyscallErrno(e wasiabi.Errno) syscall.Errno {
	switch e {
	case wasiabi.EPIPE:
		return syscall.EPIPE
	case wasiabi.ECONNRESET:
		return syscall.ECONNRESET
	case wasiabi.ECONNREFUSED:
		return syscall.ECONNREFUSED
	case wasiabi.ENOENT:
		return syscall.ENOENT
	case wasiabi.ENOTDIR:
		return syscall.ENOTDIR
	case wasiabi.ELOOP:
		return syscall.ELOOP
	case wasiabi.ENAMETOOLONG:
		return syscall.ENAMETOOLONG
	case wasiabi.EACCES:
		return syscall.EACCES
	case wasiabi.EINVAL:
		return syscall.EINVAL
	case wasiabi.ENOTCONN:
		return syscall.ENOTCONN
	case wasiabi.EISCONN:
		return syscall.EISCONN
	case wasiabi.EBADF:
		return syscall.EBADF
	}
	return syscall.EIO
}

// connDeadline is a settable deadline. wait returns a channel that is
// closed once the deadline passes. set replaces the deadline and wakes
// waiters if the new one is already past. It is safe for concurrent use.
type connDeadline struct {
	mu    sync.Mutex
	timer *time.Timer
	ch    chan struct{} // closed when the deadline has passed
}

// set makes t the deadline, and the zero time clears it. A waiter holding
// the current channel sees the change, because the channel stays open
// unless t has passed. Once closed is closed, set does nothing and returns
// false. set makes that check under d.mu. So if a set races with Close
// and arms a timer, Close's stop disarms it, because stop comes after
// Close closes closed.
func (d *connDeadline) set(t time.Time, closed <-chan struct{}) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-closed:
		return false
	default:
	}
	d.stopLocked()
	var dur time.Duration
	if !t.IsZero() {
		dur = time.Until(t)
	}
	past := !t.IsZero() && dur <= 0
	if d.ch == nil || (!past && isClosed(d.ch)) {
		d.ch = make(chan struct{})
	}
	switch {
	case t.IsZero():
	case past:
		if !isClosed(d.ch) {
			close(d.ch)
		}
	default:
		ch := d.ch
		var tm *time.Timer
		tm = time.AfterFunc(dur, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			// A later set replaced this timer, so its deadline no longer
			// holds.
			if d.timer == tm && !isClosed(ch) {
				close(ch)
			}
		})
		d.timer = tm
	}
	return true
}

// wait returns a channel closed once the deadline has passed.
func (d *connDeadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.ch
}

// stop disarms the timer, leaving the channel as it is.
func (d *connDeadline) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopLocked()
}

func (d *connDeadline) stopLocked() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
