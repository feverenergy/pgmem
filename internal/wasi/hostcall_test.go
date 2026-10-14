package wasi

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// fakeMem is guest memory backed by a slice, implementing what the host
// functions use.
type fakeMem struct {
	api.Memory
	b []byte
}

func (m *fakeMem) Read(off, n uint32) ([]byte, bool) {
	if uint64(off)+uint64(n) > uint64(len(m.b)) {
		return nil, false
	}
	return m.b[off : off+n], true
}

func (m *fakeMem) Write(off uint32, v []byte) bool {
	b, ok := m.Read(off, uint32(len(v)))
	if ok {
		copy(b, v)
	}
	return ok
}

func (m *fakeMem) WriteUint32Le(off, v uint32) bool {
	b, ok := m.Read(off, 4)
	if ok {
		binary.LittleEndian.PutUint32(b, v)
	}
	return ok
}

func (m *fakeMem) WriteUint64Le(off uint32, v uint64) bool {
	b, ok := m.Read(off, 8)
	if ok {
		binary.LittleEndian.PutUint64(b, v)
	}
	return ok
}

// Guest memory used by the host call tests. It is 1 KiB and holds:
//
//   - a valid iovec at 0, for 16 bytes at 64;
//   - an iovec running off the end at 8;
//   - the paths "newfile" at 200, "dev" at 300 and "lnk" (a symlink) at
//     310.
const (
	memSize    = 1024
	outOfRange = 1 << 20
	newfileAt  = 200
	devAt      = 300
	lnkAt      = 310
)

// hostProc returns a process with the root fds plus /dev/zero at fd 4.
func hostProc() (*Proc, *fakeMem) {
	fs := memfs.New()
	p := &Proc{FS: fs, FDs: NewRootFDs(fs, nil, nil, nil), Args: []string{"/prog"}}
	zero, _ := fs.Lookup(fs.Root(), "/dev/zero", true)
	p.FDs.InstallAt(4, NewDesc(NewNodeFile(fs, zero), 0))
	fs.Symlink("dev", fs.Root(), "/lnk")
	mem := &fakeMem{b: make([]byte, memSize)}
	le.PutUint32(mem.b[0:], 64)
	le.PutUint32(mem.b[4:], 16)
	le.PutUint32(mem.b[8:], memSize-4)
	le.PutUint32(mem.b[12:], 16)
	copy(mem.b[newfileAt:], "newfile")
	copy(mem.b[devAt:], "dev")
	copy(mem.b[lnkAt:], "lnk")
	return p, mem
}

// invoke runs a host function with the given argument slots.
func invoke(p *Proc, mem *fakeMem, fn func(*call) wasiabi.Errno, args ...uint64) wasiabi.Errno {
	stack := make([]uint64, max(len(args), 1))
	copy(stack, args)
	c := call{ctx: context.Background(), mem: mem, p: p, stack: stack}
	return fn(&c)
}

// TestGuestCannotCrashTheHost feeds host functions bad pointers and sizes.
// Each must answer an errno, and none may leak an fd.
func TestGuestCannotCrashTheHost(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*call) wasiabi.Errno
		args []uint64
		want wasiabi.Errno
	}{
		{"fd_read iovec array", fdRead, []uint64{4, outOfRange, 1, 100}, wasiabi.EFAULT},
		{"fd_read iovec buffer", fdRead, []uint64{4, 8, 1, 100}, wasiabi.EFAULT},
		{"fd_read result", fdRead, []uint64{4, 0, 1, outOfRange}, wasiabi.EFAULT},
		{"fd_pwrite iovec array", fdPwrite, []uint64{4, outOfRange, 1, 0, 100}, wasiabi.EFAULT},
		{"fd_write iovec count", fdWrite, []uint64{1, 0, 1 << 20, 100}, wasiabi.EINVAL},
		{"fd_read closed fd", fdRead, []uint64{9, 0, 1, 100}, wasiabi.EBADF},
		{"fd_seek whence", fdSeek, []uint64{4, 0, 7, 100}, wasiabi.EINVAL},
		{"fd_seek result", fdSeek, []uint64{4, 0, 0, outOfRange}, wasiabi.EFAULT},
		{"fd_fdstat_get result", fdFdstatGet, []uint64{1, outOfRange}, wasiabi.EFAULT},
		{"fd_prestat_dir_name buffer", fdPrestatDirName, []uint64{3, outOfRange, 1}, wasiabi.EFAULT},
		{"fd_readdir buffer", fdReaddir, []uint64{3, outOfRange, 64, 0, 128}, wasiabi.EFAULT},
		{"fd_readdir huge cookie", fdReaddir, []uint64{3, 0, 64, 1 << 63, 128}, wasiabi.ESUCCESS},
		{"path_open path", pathOpen, []uint64{3, 0, outOfRange, 7, 0, 0, 0, 0, 100}, wasiabi.EFAULT},
		{"path_open result", pathOpen, []uint64{3, 0, newfileAt, 7, uint64(wasiabi.OflagCreat), 0, 0, 0, outOfRange}, wasiabi.EFAULT},
		{"path_filestat_get result", pathFilestatGet, []uint64{3, 1, devAt, 3, outOfRange}, wasiabi.EFAULT},
		{"path_open path too long", pathOpen, []uint64{3, 0, 0, 4 << 20, 0, 0, 0, 0, 100}, wasiabi.ENAMETOOLONG},
		{"path_rename new path too long", pathRename, []uint64{3, newfileAt, 7, 3, 0, 1 << 31}, wasiabi.ENAMETOOLONG},
		{"path_symlink target too long", pathSymlink, []uint64{0, 4 << 20, 3, newfileAt, 7}, wasiabi.ENAMETOOLONG},
		{"path_readlink buffer", pathReadlink, []uint64{3, lnkAt, 3, outOfRange, 8, 100}, wasiabi.EFAULT},
		{"args_get pointers", argsGet, []uint64{outOfRange, 0}, wasiabi.EFAULT},
		{"args_sizes_get result", argsSizesGet, []uint64{outOfRange, 0}, wasiabi.EFAULT},
		{"clock_time_get result", clockTimeGet, []uint64{0, 0, outOfRange}, wasiabi.EFAULT},
		{"random_get buffer", randomGet, []uint64{memSize - 4, 100}, wasiabi.EFAULT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, mem := hostProc()
			if got := invoke(p, mem, tc.fn, tc.args...); got != tc.want {
				t.Fatalf("errno %d, want %d", got, tc.want)
			}
			if n := p.FDs.Len(); n != 5 {
				t.Fatalf("%d fds open, want 5", n)
			}
		})
	}
}

func TestReaddirPastTheEndIsEmpty(t *testing.T) {
	p, mem := hostProc()
	le.PutUint32(mem.b[128:], 99)
	if err := invoke(p, mem, fdReaddir, 3, 0, 64, 1<<63, 128); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	if used := le.Uint32(mem.b[128:]); used != 0 {
		t.Fatalf("bufused %d", used)
	}
}

// TestNoProcIsENOSYS calls every function through its wazero wrapper on a
// context with no Proc.
func TestNoProcIsENOSYS(t *testing.T) {
	for _, f := range functions {
		stack := make([]uint64, max(len(f.params), 1))
		wrap(f.fn)(context.Background(), nil, stack)
		if wasiabi.Errno(stack[0]) != wasiabi.ENOSYS {
			t.Errorf("%s: errno %d", f.name, stack[0])
		}
	}
}

func TestRenumberHostCall(t *testing.T) {
	p, mem := hostProc()
	zero, _ := p.FDs.Get(4)
	if err := invoke(p, mem, fdRenumber, 4, 1); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	if _, err := p.FDs.Get(4); err != wasiabi.EBADF {
		t.Fatal("fd 4 still open")
	}
	if d, _ := p.FDs.Get(1); d != zero {
		t.Fatal("fd 1 is not the renumbered description")
	}
	if err := invoke(p, mem, fdRenumber, 4, 1); err != wasiabi.EBADF {
		t.Fatalf("renumber from a closed fd: %d", err)
	}
}

// fd_read with two iovecs whose first one the pipe fills exactly returns
// those bytes instead of blocking for the second.
func TestReadvStopsWhenPipeRunsDry(t *testing.T) {
	p, mem := hostProc()
	r, w := NewPipe()
	defer w.Close()
	p.FDs.InstallAt(5, NewDesc(r, 0))
	w.Write([]byte("abc"), 0)
	le.PutUint32(mem.b[800:], 840)
	le.PutUint32(mem.b[804:], 3)
	le.PutUint32(mem.b[808:], 850)
	le.PutUint32(mem.b[812:], 8)
	done := make(chan wasiabi.Errno, 1)
	go func() { done <- invoke(p, mem, fdRead, 5, 800, 2, 870) }()
	select {
	case err := <-done:
		if err != wasiabi.ESUCCESS || le.Uint32(mem.b[870:]) != 3 || string(mem.b[840:843]) != "abc" {
			t.Fatalf("errno %d nread %d %q", err, le.Uint32(mem.b[870:]), mem.b[840:843])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fd_read blocked on the second iovec")
	}
}

// fakeModule is the caller of a wrapped host function. It records
// CloseWithExitCode.
type fakeModule struct {
	api.Module
	mem    *fakeMem
	closed *uint32
}

func (m fakeModule) Memory() api.Memory { return m.mem }

func (m fakeModule) CloseWithExitCode(_ context.Context, code uint32) error {
	*m.closed = code
	return nil
}

// callRecover runs f and returns what it panicked with, or nil.
func callRecover(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

// TestKilledProcessExitsAtHostCall checks that once a process's context is
// done, a host call does not return, with EINTR or anything else. It ends
// the instance with KillExitCode instead, whether the context ended before
// the call or while it blocked.
func TestKilledProcessExitsAtHostCall(t *testing.T) {
	blocking := func(c *call) wasiabi.Errno {
		<-c.ctx.Done()
		return wasiabi.EINTR
	}
	for _, tc := range []struct {
		name   string
		cancel bool // before the call
		fn     func(*call) wasiabi.Errno
	}{
		{"cancelled before", true, fdClose},
		{"cancelled while blocked", false, blocking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mem := hostProc()
			ctx, cancel := context.WithCancel(WithProc(context.Background(), p))
			defer cancel()
			if tc.cancel {
				cancel()
			} else {
				time.AfterFunc(10*time.Millisecond, cancel)
			}
			var code uint32 = 1 << 31
			mod := fakeModule{mem: mem, closed: &code}
			stack := []uint64{4}
			v := callRecover(func() { wrap(tc.fn)(ctx, mod, stack) })
			if v == nil {
				t.Fatalf("host call returned errno %d", stack[0])
			}
			if exitErr, ok := v.(*sys.ExitError); !ok || exitErr.ExitCode() != KillExitCode {
				t.Fatalf("panicked with %v, want exit %d", v, KillExitCode)
			}
			if code != KillExitCode {
				t.Fatalf("module closed with %d", code)
			}
			if _, err := p.FDs.Get(4); tc.cancel && err != wasiabi.ESUCCESS {
				t.Fatal("the call ran although the process was already killed")
			}
		})
	}
}

// TestPathLengthLimits checks that a guest path is read only up to
// memfs.MaxPathLen bytes, and a symlink target only up to
// memfs.MaxSymlinkLen. Longer ones fail with ENAMETOOLONG. The cases above
// show that the length is checked before the memory is read.
func TestPathLengthLimits(t *testing.T) {
	p, _ := hostProc()
	mem := &fakeMem{b: make([]byte, 3*memfs.MaxPathLen)}
	path := func(n int) uint64 { // "a/a/..." of n bytes at 0
		for i := range n {
			mem.b[i] = "a/"[i%2]
		}
		return uint64(n)
	}
	if err := invoke(p, mem, pathCreateDirectory, 3, 0, path(memfs.MaxPathLen)); err != wasiabi.ENOENT {
		t.Fatalf("path of %d bytes: errno %d, want ENOENT", memfs.MaxPathLen, err)
	}
	if err := invoke(p, mem, pathCreateDirectory, 3, 0, path(memfs.MaxPathLen+1)); err != wasiabi.ENAMETOOLONG {
		t.Fatalf("path of %d bytes: errno %d, want ENAMETOOLONG", memfs.MaxPathLen+1, err)
	}
	copy(mem.b[2*memfs.MaxPathLen:], "lnk2")
	at := uint64(2 * memfs.MaxPathLen)
	if err := invoke(p, mem, pathSymlink, 0, path(memfs.MaxSymlinkLen+1), 3, at, 4); err != wasiabi.ENAMETOOLONG {
		t.Fatalf("target of %d bytes: errno %d, want ENAMETOOLONG", memfs.MaxSymlinkLen+1, err)
	}
	if err := invoke(p, mem, pathSymlink, 0, path(memfs.MaxSymlinkLen), 3, at, 4); err != wasiabi.ESUCCESS {
		t.Fatalf("target of %d bytes: errno %d", memfs.MaxSymlinkLen, err)
	}
}

// pipeProc returns hostProc's process with a pipe whose read end is fd 5
// and write end fd 6, and guest memory big enough for an iovec at 0
// covering bufAt..bufAt+n.
func pipeProc(n uint32) (*Proc, *fakeMem, File, File) {
	p, _ := hostProc()
	r, w := NewPipe()
	p.FDs.InstallAt(5, NewDesc(r, 0))
	p.FDs.InstallAt(6, NewDesc(w, 0))
	mem := &fakeMem{b: make([]byte, bufAt+n)}
	le.PutUint32(mem.b[0:], bufAt)
	le.PutUint32(mem.b[4:], n)
	return p, mem, r, w
}

// bufAt is where pipeProc's iovec points. The count goes at 16.
const bufAt = 64

// blockedCall runs fn on another goroutine, checks that it is still
// blocked after a moment, runs wake, and returns fn's errno.
func blockedCall(t *testing.T, fn func() wasiabi.Errno, wake func()) wasiabi.Errno {
	t.Helper()
	done := make(chan wasiabi.Errno, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		t.Fatalf("returned without blocking: errno %d", err)
	case <-time.After(20 * time.Millisecond):
	}
	wake()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("still blocked after the wake-up")
	}
	return 0
}

// Closing the write end while fd_write is blocked on it wakes the call
// with EBADF. This is an fd_close racing the call on another thread.
func TestCloseWakesBlockedWriteOnSameEnd(t *testing.T) {
	p, mem, r, w := pipeProc(1)
	defer r.Close()
	if n, err := w.Write(make([]byte, PipeCapacity), 0); n != PipeCapacity || err != 0 {
		t.Fatalf("fill: %d %d", n, err)
	}
	write := func() wasiabi.Errno { return invoke(p, mem, fdWrite, 6, 0, 1, 16) }
	if err := blockedCall(t, write, func() { w.Close() }); err != wasiabi.EBADF {
		t.Fatalf("errno %d, want EBADF", err)
	}
}

// Closing the read end wakes a blocked fd_write. One that got some bytes
// in reports them, and the next one fails with EPIPE.
func TestCloseWakesBlockedWriter(t *testing.T) {
	p, mem, r, _ := pipeProc(PipeCapacity + 10)
	write := func() wasiabi.Errno { return invoke(p, mem, fdWrite, 6, 0, 1, 16) }
	if err := blockedCall(t, write, func() { r.Close() }); err != 0 || le.Uint32(mem.b[16:]) != PipeCapacity {
		t.Fatalf("errno %d, wrote %d, want the %d bytes that fit", err, le.Uint32(mem.b[16:]), PipeCapacity)
	}
	if err := write(); err != wasiabi.EPIPE {
		t.Fatalf("write after the reader closed: errno %d, want EPIPE", err)
	}
}

// A blocking fd_write larger than the pipe completes as the reader drains.
func TestBlockingWriteCompletes(t *testing.T) {
	const size = 3*PipeCapacity + 123
	p, mem, r, w := pipeProc(size)
	defer w.Close()
	got := make(chan int, 1)
	go func() {
		total := 0
		buf := make([]byte, 5000)
		for total < size {
			n, err := r.Read(buf, 0)
			if err == wasiabi.EAGAIN {
				time.Sleep(time.Millisecond)
				continue
			}
			total += n
		}
		got <- total
	}()
	if err := invoke(p, mem, fdWrite, 6, 0, 1, 16); err != 0 || le.Uint32(mem.b[16:]) != size {
		t.Fatalf("errno %d, wrote %d of %d", err, le.Uint32(mem.b[16:]), size)
	}
	if n := <-got; n != size {
		t.Fatalf("reader got %d", n)
	}
}

// fd_read and fd_write on a non-blocking description fail with EAGAIN
// instead of waiting.
func TestNonblockingDescNeverWaits(t *testing.T) {
	p, mem, r, w := pipeProc(1)
	d5, _ := p.FDs.Get(5)
	d5.SetFlags(wasiabi.FdflagNonblock)
	if err := invoke(p, mem, fdRead, 5, 0, 1, 16); err != wasiabi.EAGAIN {
		t.Fatalf("read: errno %d, want EAGAIN", err)
	}
	w.Write(make([]byte, PipeCapacity), 0)
	d6, _ := p.FDs.Get(6)
	d6.SetFlags(wasiabi.FdflagNonblock)
	if err := invoke(p, mem, fdWrite, 6, 0, 1, 16); err != wasiabi.EAGAIN {
		t.Fatalf("write: errno %d, want EAGAIN", err)
	}
	r.Close()
	w.Close()
}

// testSignals is a Signals whose pending state the test sets.
type testSignals struct {
	pending atomic.Bool
	wake    chan struct{}
}

func newTestSignals() *testSignals { return &testSignals{wake: make(chan struct{}, 1)} }

func (s *testSignals) Deliverable() bool     { return s.pending.Load() }
func (s *testSignals) Wake() <-chan struct{} { return s.wake }
func (s *testSignals) send() {
	s.pending.Store(true)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// A deliverable signal interrupts a blocked fd_read and fd_write with
// EINTR. A write that already moved bytes reports them instead.
func TestSignalInterruptsBlockedReadAndWrite(t *testing.T) {
	p, mem, r, w := pipeProc(PipeCapacity + 10)
	defer r.Close()
	defer w.Close()
	sig := newTestSignals()
	p.Signals = sig
	read := func() wasiabi.Errno { return invoke(p, mem, fdRead, 5, 0, 1, 16) }
	if err := blockedCall(t, read, sig.send); err != wasiabi.EINTR {
		t.Fatalf("read: errno %d, want EINTR", err)
	}
	sig.pending.Store(false)
	write := func() wasiabi.Errno { return invoke(p, mem, fdWrite, 6, 0, 1, 16) }
	if err := blockedCall(t, write, sig.send); err != 0 || le.Uint32(mem.b[16:]) != PipeCapacity {
		t.Fatalf("write: errno %d, wrote %d, want %d", err, le.Uint32(mem.b[16:]), PipeCapacity)
	}
	sig.pending.Store(false)
	le.PutUint32(mem.b[4:], 1)
	if err := blockedCall(t, write, sig.send); err != wasiabi.EINTR {
		t.Fatalf("write to a full pipe: errno %d, want EINTR", err)
	}
}

// Two goroutines bounce a byte through two pipes with blocking fd_read and
// fd_write. Every round's wake-up is the only one that round gets, so a
// single lost wake-up stalls the exchange.
func TestPingPongNeverStalls(t *testing.T) {
	p, _ := hostProc()
	r1, w1 := NewPipe()
	r2, w2 := NewPipe()
	for fd, f := range map[int32]File{5: r1, 6: w1, 7: r2, 8: w2} {
		p.FDs.InstallAt(fd, NewDesc(f, 0))
	}
	newMem := func() *fakeMem {
		mem := &fakeMem{b: make([]byte, 128)}
		le.PutUint32(mem.b[0:], bufAt)
		le.PutUint32(mem.b[4:], 1)
		return mem
	}
	const rounds = 2000
	bounce := func(mem *fakeMem, in, out uint64, first bool) error {
		for i := 0; i < rounds; i++ {
			if !first || i > 0 {
				if err := invoke(p, mem, fdRead, in, 0, 1, 16); err != 0 || le.Uint32(mem.b[16:]) != 1 {
					return fmt.Errorf("round %d read: errno %d", i, err)
				}
			}
			if err := invoke(p, mem, fdWrite, out, 0, 1, 16); err != 0 {
				return fmt.Errorf("round %d write: errno %d", i, err)
			}
		}
		return nil
	}
	done := make(chan error, 2)
	go func() { done <- bounce(newMem(), 7, 6, true) }()
	go func() { done <- bounce(newMem(), 5, 8, false) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("stalled: a wake-up was lost")
		}
	}
}

// sock_send and sock_recv move bytes between the ends of a socket pair.
// sock_shutdown half-closes it.
func TestSockRecvSendShutdown(t *testing.T) {
	p, mem := hostProc()
	a, b := NewSockPair()
	defer a.Close()
	defer b.Close()
	p.FDs.InstallAt(5, NewDesc(a, 0))
	p.FDs.InstallAt(6, NewDesc(b, wasiabi.FdflagNonblock))
	r, w := NewPipe()
	defer w.Close()
	p.FDs.InstallAt(7, NewDesc(r, 0))

	// Memory layout:
	//
	//   - a send iovec at 100 for "hi" at 120;
	//   - a receive iovec at 0, for 16 bytes at 64;
	//   - two receive iovecs at 400, for 1 byte at 440 and 8 at 450;
	//   - counts at 200 and ro_flags at 204.
	le.PutUint32(mem.b[100:], 120)
	le.PutUint32(mem.b[104:], 2)
	copy(mem.b[120:], "hi")
	le.PutUint32(mem.b[400:], 440)
	le.PutUint32(mem.b[404:], 1)
	le.PutUint32(mem.b[408:], 450)
	le.PutUint32(mem.b[412:], 8)

	if err := invoke(p, mem, sockSend, 5, 100, 1, 0, 200); err != 0 || le.Uint32(mem.b[200:]) != 2 {
		t.Fatalf("sock_send: errno %d, sent %d", err, le.Uint32(mem.b[200:]))
	}
	recv := func(iovs, n, flags uint64) (string, wasiabi.Errno) {
		clear(mem.b[64:80])
		clear(mem.b[440:460])
		le.PutUint32(mem.b[200:], 99)
		le.PutUint16(mem.b[204:], 0xffff)
		err := invoke(p, mem, sockRecv, 6, iovs, n, flags, 200, 204)
		got := le.Uint32(mem.b[200:])
		if err == 0 && le.Uint16(mem.b[204:]) != 0 {
			t.Fatalf("ro_flags %#x", le.Uint16(mem.b[204:]))
		}
		if iovs == 400 {
			return string(mem.b[440:441]) + string(mem.b[450:450+max(got, 1)-1]), err
		}
		return string(mem.b[64 : 64+got]), err
	}
	if s, err := recv(400, 2, uint64(wasiabi.RiflagsRecvPeek)); err != 0 || s != "hi" {
		t.Fatalf("peek across two iovecs: %q errno %d", s, err)
	}
	if s, err := recv(0, 1, uint64(wasiabi.RiflagsRecvPeek)); err != 0 || s != "hi" {
		t.Fatalf("peek: %q errno %d", s, err)
	}
	if s, err := recv(0, 1, 0); err != 0 || s != "hi" {
		t.Fatalf("recv: %q errno %d", s, err)
	}
	if _, err := recv(0, 1, 0); err != wasiabi.EAGAIN {
		t.Fatalf("recv on empty non-blocking socket: errno %d, want EAGAIN", err)
	}
	// A bad ro_datalen or ro_flags pointer fails before any byte is consumed.
	a.Write([]byte("ok"), 0)
	if err := invoke(p, mem, sockRecv, 6, 0, 1, 0, outOfRange, 204); err != wasiabi.EFAULT {
		t.Fatalf("recv with bad ro_datalen: errno %d, want EFAULT", err)
	}
	if err := invoke(p, mem, sockRecv, 6, 0, 1, 0, 200, outOfRange); err != wasiabi.EFAULT {
		t.Fatalf("recv with bad ro_flags: errno %d, want EFAULT", err)
	}
	if s, err := recv(0, 1, 0); err != 0 || s != "ok" {
		t.Fatalf("recv after the faults: %q errno %d", s, err)
	}
	if err := invoke(p, mem, sockShutdown, 5, uint64(wasiabi.SdflagsWr)); err != 0 {
		t.Fatalf("sock_shutdown: errno %d", err)
	}
	if s, err := recv(0, 1, 0); err != 0 || s != "" || le.Uint32(mem.b[200:]) != 0 {
		t.Fatalf("recv after peer SHUT_WR: %q errno %d", s, err)
	}
	if err := invoke(p, mem, sockRecv, 7, 0, 1, 0, 200, 204); err != wasiabi.ENOTSOCK {
		t.Fatalf("sock_recv on a pipe: errno %d, want ENOTSOCK", err)
	}
	if err := invoke(p, mem, sockShutdown, 7, uint64(wasiabi.SdflagsWr)); err != wasiabi.ENOTSOCK {
		t.Fatalf("sock_shutdown on a pipe: errno %d, want ENOTSOCK", err)
	}
	if err := invoke(p, mem, sockRecv, 6, 0, 1, uint64(wasiabi.RiflagsRecvWaitall), 200, 204); err != wasiabi.ENOTSUP {
		t.Fatalf("sock_recv with WAITALL: errno %d, want ENOTSUP", err)
	}
	if err := invoke(p, mem, sockSend, 6, 100, 1, 1, 200); err != wasiabi.EINVAL {
		t.Fatalf("sock_send with flags: errno %d, want EINVAL", err)
	}
}

// A blocking sock_recv waits for data and wakes when the peer sends.
func TestSockRecvBlocks(t *testing.T) {
	p, mem := hostProc()
	a, b := NewSockPair()
	defer a.Close()
	defer b.Close()
	p.FDs.InstallAt(5, NewDesc(a, 0))
	recv := func() wasiabi.Errno { return invoke(p, mem, sockRecv, 5, 0, 1, 0, 200, 204) }
	if err := blockedCall(t, recv, func() { b.Write([]byte("x"), 0) }); err != 0 || le.Uint32(mem.b[200:]) != 1 {
		t.Fatalf("errno %d, received %d", err, le.Uint32(mem.b[200:]))
	}
}
