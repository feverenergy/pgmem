package wasi

import "github.com/feverenergy/pgmem/internal/wasiabi"

// sockArg returns the description and socket of the fd in argument 0. It
// fails with ENOTSOCK if the file is not a socket.
func (c *call) sockArg() (*Desc, *Sock, wasiabi.Errno) {
	d, err := c.desc(0)
	if err != wasiabi.ESUCCESS {
		return nil, nil, err
	}
	s, ok := d.file.(*Sock)
	if !ok {
		return nil, nil, wasiabi.ENOTSOCK
	}
	return d, s, wasiabi.ESUCCESS
}

// sockRecv implements sock_recv(fd, ri_data, ri_data_len, ri_flags,
// *ro_datalen, *ro_flags). It reads from a connected socket like fd_read.
// RiflagsRecvPeek leaves the data queued. RiflagsRecvWaitall is not
// supported (ENOTSUP). ro_flags is always 0, because a stream never
// truncates. A file that is not a socket gives ENOTSOCK.
func sockRecv(c *call) wasiabi.Errno {
	d, s, err := c.sockArg()
	if err != wasiabi.ESUCCESS {
		return err
	}
	flags := c.u32(3)
	switch {
	case flags&uint32(wasiabi.RiflagsRecvWaitall) != 0:
		return wasiabi.ENOTSUP
	case flags&^uint32(wasiabi.RiflagsRecvPeek) != 0:
		return wasiabi.EINVAL
	}
	bufs, err := c.iovecs(c.u32(1), c.u32(2))
	if err != wasiabi.ESUCCESS {
		return err
	}
	// Write both results up front, so a bad pointer fails before any byte
	// is consumed. ro_flags (a u16) is always 0.
	if err := c.putU32(c.u32(4), 0); err != wasiabi.ESUCCESS {
		return err
	}
	if err := c.put(c.u32(5), []byte{0, 0}); err != wasiabi.ESUCCESS {
		return err
	}
	var n int64
	if flags&uint32(wasiabi.RiflagsRecvPeek) != 0 {
		n, err = c.peekv(d, s, bufs)
	} else {
		n, err = c.readv(d, bufs)
	}
	return c.putCount(4, n, err)
}

// peekv peeks into bufs. Each peek starts at the first buffered byte, so
// it peeks once, into a buffer no bigger than the socket's, and scatters
// what it got. Like fd_read, a blocking description waits for data.
func (c *call) peekv(d *Desc, s *Sock, bufs [][]byte) (int64, wasiabi.Errno) {
	total := 0
	for _, b := range bufs {
		total += len(b)
	}
	if total == 0 {
		return 0, wasiabi.ESUCCESS
	}
	tmp := make([]byte, min(total, SockCapacity))
	n, err := c.blocking(s, d.Flags(), func() (int, wasiabi.Errno) { return s.peek(tmp) })
	if err != wasiabi.ESUCCESS {
		return 0, err
	}
	rest := tmp[:n]
	for _, b := range bufs {
		rest = rest[copy(b, rest):]
	}
	return int64(n), wasiabi.ESUCCESS
}

// sockSend implements sock_send(fd, si_data, si_data_len, si_flags,
// *so_datalen). It writes to a connected socket like fd_write. si_flags
// must be 0, or the call fails with EINVAL.
func sockSend(c *call) wasiabi.Errno {
	d, _, err := c.sockArg()
	if err != wasiabi.ESUCCESS {
		return err
	}
	if c.u32(3) != 0 {
		return wasiabi.EINVAL
	}
	bufs, err := c.iovecs(c.u32(1), c.u32(2))
	if err != wasiabi.ESUCCESS {
		return err
	}
	n, err := c.writev(d, bufs)
	return c.putCount(4, n, err)
}

// sockShutdown implements sock_shutdown(fd, how). how is SdflagsRd,
// SdflagsWr or both.
func sockShutdown(c *call) wasiabi.Errno {
	_, s, err := c.sockArg()
	if err != wasiabi.ESUCCESS {
		return err
	}
	how := c.u32(1)
	if how > 0xff {
		return wasiabi.EINVAL
	}
	return s.Shutdown(uint8(how))
}
