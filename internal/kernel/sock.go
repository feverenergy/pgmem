package kernel

import (
	"context"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// Flags of pgm.sock_open and sock_accept. Compat maps SOCK_NONBLOCK and
// SOCK_CLOEXEC onto them.
const (
	sockNonblock = 1
	sockCloexec  = 2
)

// maxSockPath is the longest socket path, which is sun_path less its NUL.
const maxSockPath = 107

// sockRegistry maps the file a socket is bound to onto the socket, so a
// connect finds its listener by path. Its key is the node, so once the
// path is unlinked the socket is unreachable, as on Linux.
type sockRegistry struct {
	mu    sync.Mutex
	socks map[*memfs.Node]*wasi.Sock
}

func newSockRegistry() *sockRegistry {
	return &sockRegistry{socks: map[*memfs.Node]*wasi.Sock{}}
}

func (r *sockRegistry) add(n *memfs.Node, s *wasi.Sock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.socks[n] = s
}

// remove deletes n's entry only while it still maps to s.
func (r *sockRegistry) remove(n *memfs.Node, s *wasi.Sock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.socks[n] == s {
		delete(r.socks, n)
	}
}

func (r *sockRegistry) lookup(n *memfs.Node) (*wasi.Sock, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.socks[n]
	return s, ok
}

// sock returns the socket and description of the fd in argument i. It
// fails with EBADF if the fd is not open, and with ENOTSOCK if it is not a
// socket.
func (c *pgmCall) sock(i int) (*wasi.Sock, *wasi.Desc, wasiabi.Errno) {
	d, errno := c.p.wasi.FDs.Get(c.i32(i))
	if errno != wasiabi.ESUCCESS {
		return nil, nil, errno
	}
	s, ok := d.File().(*wasi.Sock)
	if !ok {
		return nil, nil, wasiabi.ENOTSOCK
	}
	return s, d, wasiabi.ESUCCESS
}

// sockPath reads the socket path whose pointer and length are arguments i
// and i+1. It fails with EINVAL unless the path is absolute, at most
// maxSockPath bytes and free of NULs.
func (c *pgmCall) sockPath(i int) (string, wasiabi.Errno) {
	n := c.u32(i + 1)
	if n == 0 || n > maxSockPath {
		return "", wasiabi.EINVAL
	}
	b, ok := c.mem.Read(c.u32(i), n)
	if !ok {
		return "", wasiabi.EFAULT
	}
	path := string(b)
	if path[0] != '/' || strings.IndexByte(path, 0) >= 0 {
		return "", wasiabi.EINVAL
	}
	return path, wasiabi.ESUCCESS
}

// sockFlags converts the flags of sock_open and sock_accept into the new
// description's fdflags and its close-on-exec flag. Other bits fail with
// EINVAL.
func sockFlags(flags int32) (fdflags uint16, cloexec bool, errno wasiabi.Errno) {
	if flags&^(sockNonblock|sockCloexec) != 0 {
		return 0, false, wasiabi.EINVAL
	}
	if flags&sockNonblock != 0 {
		fdflags = wasiabi.FdflagNonblock
	}
	return fdflags, flags&sockCloexec != 0, wasiabi.ESUCCESS
}

// installSock installs s with flags, as for sock_open, and returns its fd.
// On failure it closes s.
func (c *pgmCall) installSock(s *wasi.Sock, flags int32) int32 {
	fdflags, cloexec, errno := sockFlags(flags)
	if errno != wasiabi.ESUCCESS {
		s.Close()
		return fail(errno)
	}
	fds := c.p.wasi.FDs
	fd, errno := fds.Install(wasi.NewDesc(s, fdflags)) // EMFILE closes s
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	if cloexec {
		fds.SetCloexec(fd, true)
	}
	return fd
}

// pgmSockOpen implements sock_open(flags) -> fd | -errno. It returns a
// fresh, unbound Unix stream socket. Flag 1 is non-blocking and flag 2 is
// close-on-exec.
func pgmSockOpen(c *pgmCall) int32 {
	return c.installSock(wasi.NewSock(), c.i32(0))
}

// pgmSockBind implements sock_bind(fd, path, len) -> 0 | -errno. It binds
// the socket to path and creates a placeholder file there. path is len
// bytes, absolute, with no NUL and at most 107 bytes. It fails with
// EADDRINUSE if the path exists.
func pgmSockBind(c *pgmCall) int32 {
	s, _, errno := c.sock(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	path, errno := c.sockPath(1)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	// Call CanBind before creating and registering the path, so a bound
	// socket never becomes reachable at a second path, however briefly.
	if errno := s.CanBind(); errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	fs, root := c.k.fs, c.k.fs.Root()
	n, errno := fs.Open(root, path, memfs.OpenFlags{Create: true, Excl: true})
	switch errno {
	case wasiabi.ESUCCESS:
	case wasiabi.EEXIST:
		return fail(wasiabi.EADDRINUSE)
	default:
		return fail(errno)
	}
	// Register before Bind, so a close racing with it cannot leave a stale
	// entry behind. That close runs onClose. Bind can still fail if another
	// thread bound or closed s since CanBind.
	reg := c.k.socks
	reg.add(n, s)
	if errno := s.Bind(path, func() { reg.remove(n, s) }); errno != wasiabi.ESUCCESS {
		reg.remove(n, s)
		// This removes by path, whatever the path names now, so it is best
		// effort.
		_ = fs.Unlink(root, path)
		return fail(errno)
	}
	return 0
}

// pgmSockListen implements sock_listen(fd, backlog) -> 0 | -errno.
func pgmSockListen(c *pgmCall) int32 {
	s, _, errno := c.sock(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return fail(s.Listen(int(c.i32(1))))
}

// pgmSockConnect implements sock_connect(fd, path, len) -> 0 | -errno. It
// connects to the socket listening at path. It fails with ENOENT if there
// is no such file, and with ECONNREFUSED if no socket listens on it. While
// the listener's queue is full, a blocking socket waits, and a signal
// interrupts it with EINTR. A non-blocking socket fails with EAGAIN.
func pgmSockConnect(c *pgmCall) int32 {
	s, d, errno := c.sock(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	path, errno := c.sockPath(1)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	n, errno := c.k.fs.Lookup(c.k.fs.Root(), path, true)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	l, ok := c.k.socks.lookup(n)
	if !ok {
		return fail(wasiabi.ECONNREFUSED)
	}
	for {
		// Get the channel before the attempt, so an accept in between
		// is not lost.
		changed := l.Changed()
		errno := s.Connect(l)
		if errno != wasiabi.EAGAIN || d.Flags()&wasiabi.FdflagNonblock != 0 {
			return fail(errno)
		}
		if errno := c.p.wasi.Wait(c.ctx, time.Time{}, changed); errno != wasiabi.ESUCCESS {
			return fail(errno)
		}
	}
}

// pgmSockAccept implements sock_accept(fd, flags) -> fd | -errno. It takes
// the oldest queued connection and installs it with flags, as for
// sock_open. With none queued, a blocking socket waits, and a signal
// interrupts it with EINTR. A non-blocking socket fails with EAGAIN.
func pgmSockAccept(c *pgmCall) int32 {
	l, d, errno := c.sock(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	if _, _, errno := sockFlags(c.i32(1)); errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	for {
		changed := l.Changed()
		s, errno := l.Accept()
		switch {
		case errno == wasiabi.ESUCCESS:
			return c.installSock(s, c.i32(1))
		case errno != wasiabi.EAGAIN || d.Flags()&wasiabi.FdflagNonblock != 0:
			return fail(errno)
		}
		if errno := c.p.wasi.Wait(c.ctx, time.Time{}, changed); errno != wasiabi.ESUCCESS {
			return fail(errno)
		}
	}
}

// pgmSockName implements sock_name(fd, which, buf, buflen) -> len |
// -errno. It writes the socket's local (which 0) or peer (which 1) address
// to buf, without a NUL and truncated to buflen, and returns its full
// length. It fails with ENOTCONN for the peer of an unconnected socket.
func pgmSockName(c *pgmCall) int32 {
	s, _, errno := c.sock(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	var addr string
	switch c.i32(1) {
	case 0:
		addr = s.Addr()
	case 1:
		if addr, errno = s.PeerAddr(); errno != wasiabi.ESUCCESS {
			return fail(errno)
		}
	default:
		return fail(wasiabi.EINVAL)
	}
	n := min(uint32(len(addr)), c.u32(3))
	if !c.mem.Write(c.u32(2), []byte(addr[:n])) {
		return fail(wasiabi.EFAULT)
	}
	return int32(len(addr))
}

// Dial connects to the guest socket listening at the absolute path and
// returns the Go end as a net.Conn. While the listener's queue is full it
// waits until there is room, ctx is done, or the kernel closes. Errors are
// *net.OpError and wrap one of these:
//
//   - ctx's error if it is already done;
//   - ENOENT if there is no such file;
//   - ECONNREFUSED if no socket listens on it;
//   - net.ErrClosed once the kernel is closed.
func (k *Kernel) Dial(ctx context.Context, path string) (net.Conn, error) {
	fail := func(err error) error {
		return &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: path, Net: "unix"}, Err: err}
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fail(syscall.EINVAL)
	}
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}
	k.mu.Lock()
	closed := k.closed
	k.mu.Unlock()
	if closed {
		return nil, fail(net.ErrClosed)
	}
	n, errno := k.fs.Lookup(k.fs.Root(), path, true)
	if errno != wasiabi.ESUCCESS {
		return nil, fail(wasi.SyscallErrno(errno))
	}
	l, ok := k.socks.lookup(n)
	if !ok {
		// A Close that began after the check above may already have
		// closed the listener and dropped its entry. See below.
		if k.ctx.Err() != nil {
			return nil, fail(net.ErrClosed)
		}
		return nil, fail(syscall.ECONNREFUSED)
	}
	c := wasi.NewSock()
	for {
		// Get the channel before the attempt, so an accept in between
		// is not lost.
		changed := l.Changed()
		switch errno := c.Connect(l); errno {
		case wasiabi.ESUCCESS:
			return wasi.NewConn(c), nil
		case wasiabi.EAGAIN:
		default:
			c.Close()
			// Close cancels k.ctx before it closes the listeners, so a
			// listener closed by a Close that began after the check above
			// reads as the kernel's closing, not as a refusal.
			if k.ctx.Err() != nil {
				return nil, fail(net.ErrClosed)
			}
			return nil, fail(wasi.SyscallErrno(errno))
		}
		select {
		case <-changed:
		case <-ctx.Done():
			c.Close()
			return nil, fail(ctx.Err())
		case <-k.ctx.Done():
			c.Close()
			return nil, fail(net.ErrClosed)
		}
	}
}
