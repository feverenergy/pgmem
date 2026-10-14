package wasi

import (
	"math"
	"time"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// Rights bits pgmem looks at. It enforces none of them.
const (
	rightFdSeek  uint64 = 1 << 2
	rightFdTell  uint64 = 1 << 5
	rightFdWrite uint64 = 1 << 6
)

// fdAdvise implements fd_advise(fd, offset, len, advice).
func fdAdvise(c *call) wasiabi.Errno {
	_, err := c.desc(0)
	return err
}

// fdAllocate implements fd_allocate(fd, offset, len). It grows the file to
// offset+len if it is shorter.
func fdAllocate(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	off, n := c.u64(1), c.u64(2)
	if off > math.MaxInt64 || n > math.MaxInt64-off {
		return wasiabi.EFBIG
	}
	st, err := f.Stat()
	if err != wasiabi.ESUCCESS {
		return err
	}
	if end := int64(off + n); end > st.Size {
		return f.SetSize(end)
	}
	return wasiabi.ESUCCESS
}

// fdClose implements fd_close(fd).
func fdClose(c *call) wasiabi.Errno { return c.p.FDs.Close(c.fd(0)) }

// fdSync implements fd_sync(fd) and fd_datasync(fd).
func fdSync(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return f.Sync()
}

// fdFdstatGet implements fd_fdstat_get(fd, *fdstat). Host streams lack the
// seek and tell rights, which is how wasi-libc's isatty recognises a
// terminal.
func fdFdstatGet(c *call) wasiabi.Errno {
	d, err := c.desc(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	rights := wasiabi.RightsAll
	if _, ok := d.file.(hostStream); ok {
		rights &^= rightFdSeek | rightFdTell
	}
	return c.put(c.u32(1), encodeFdstat(d.file.Filetype(), d.Flags(), rights))
}

// fdFdstatSetFlags implements fd_fdstat_set_flags(fd, flags).
func fdFdstatSetFlags(c *call) wasiabi.Errno {
	d, err := c.desc(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	d.SetFlags(uint16(c.u32(1)))
	return wasiabi.ESUCCESS
}

// fdFdstatSetRights implements fd_fdstat_set_rights(fd, base,
// inheriting). pgmem does not enforce rights.
func fdFdstatSetRights(c *call) wasiabi.Errno {
	_, err := c.desc(0)
	return err
}

// fdFilestatGet implements fd_filestat_get(fd, *filestat).
func fdFilestatGet(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	st, err := f.Stat()
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.put(c.u32(1), encodeFilestat(st, f.Filetype()))
}

// fdFilestatSetSize implements fd_filestat_set_size(fd, size).
func fdFilestatSetSize(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if c.u64(1) > math.MaxInt64 {
		return wasiabi.EFBIG
	}
	return f.SetSize(int64(c.u64(1)))
}

// fdFilestatSetTimes implements fd_filestat_set_times(fd, atim, mtim,
// fstflags).
func fdFilestatSetTimes(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	atim, mtim, err := times(c.u64(1), c.u64(2), uint16(c.u32(3)))
	if err != wasiabi.ESUCCESS {
		return err
	}
	return f.SetTimes(atim, mtim)
}

// times turns set_times arguments into SetTimes pointers. A nil pointer
// leaves that time unchanged.
func times(atim, mtim uint64, fst uint16) (*int64, *int64, wasiabi.Errno) {
	now := time.Now().UnixNano()
	a, errA := pick(atim, now, fst&wasiabi.FstflagAtim != 0, fst&wasiabi.FstflagAtimNow != 0)
	m, errM := pick(mtim, now, fst&wasiabi.FstflagMtim != 0, fst&wasiabi.FstflagMtimNow != 0)
	if errA != wasiabi.ESUCCESS || errM != wasiabi.ESUCCESS {
		return nil, nil, wasiabi.EINVAL
	}
	return a, m, wasiabi.ESUCCESS
}

func pick(t uint64, now int64, set, setNow bool) (*int64, wasiabi.Errno) {
	switch {
	case set && setNow:
		return nil, wasiabi.EINVAL
	case set:
		v := int64(t)
		return &v, wasiabi.ESUCCESS
	case setNow:
		return &now, wasiabi.ESUCCESS
	}
	return nil, wasiabi.ESUCCESS
}

// fdRead implements fd_read(fd, iovs, iovs_len, *nread). On a blocking
// description, a read that would block waits, through blocking, for the
// first buffer only.
func fdRead(c *call) wasiabi.Errno {
	d, bufs, err := c.descIovecs()
	if err != wasiabi.ESUCCESS {
		return err
	}
	n, err := c.readv(d, bufs)
	return c.putCount(3, n, err)
}

// readv reads d's file into bufs for fd_read and sock_recv.
func (c *call) readv(d *Desc, bufs [][]byte) (int64, wasiabi.Errno) {
	flags := d.Flags()
	return transfer(bufs, func(p []byte, done int64) (int, wasiabi.Errno) {
		if done > 0 {
			// readv returns what is there. Once a buffer is full, a pipe
			// that ran dry must not block the call for the next one.
			return d.file.Read(p, flags|wasiabi.FdflagNonblock)
		}
		return c.blocking(d.file, flags, func() (int, wasiabi.Errno) { return d.file.Read(p, flags) })
	})
}

// fdWrite implements fd_write(fd, iovs, iovs_len, *nwritten). On a
// blocking description, it writes each buffer whole and waits for room,
// through blocking, as often as it takes. An interrupted write reports the
// bytes it moved.
func fdWrite(c *call) wasiabi.Errno {
	d, bufs, err := c.descIovecs()
	if err != wasiabi.ESUCCESS {
		return err
	}
	n, err := c.writev(d, bufs)
	return c.putCount(3, n, err)
}

// writev writes bufs to d's file for fd_write and sock_send.
func (c *call) writev(d *Desc, bufs [][]byte) (int64, wasiabi.Errno) {
	flags := d.Flags()
	return transfer(bufs, func(p []byte, _ int64) (int, wasiabi.Errno) {
		if flags&wasiabi.FdflagNonblock != 0 {
			return d.file.Write(p, flags)
		}
		done := 0
		for done < len(p) {
			n, err := c.blocking(d.file, flags, func() (int, wasiabi.Errno) { return d.file.Write(p[done:], flags) })
			done += n
			if err != wasiabi.ESUCCESS || n == 0 {
				return done, err
			}
		}
		return done, wasiabi.ESUCCESS
	})
}

// blocking runs op, a read or write that never blocks itself. On a
// blocking description, while op moves nothing and fails with EAGAIN,
// blocking waits for f to change state and runs op again. It takes f's
// channel before each retry, so a change between the try and the wait is
// never lost. A file that is not Waitable never fails with EAGAIN, so its
// first result stands. A deliverable signal or the end of the call's
// context ends the wait with EINTR.
func (c *call) blocking(f File, flags uint16, op func() (int, wasiabi.Errno)) (int, wasiabi.Errno) {
	n, err := op()
	if err != wasiabi.EAGAIN || n > 0 || flags&wasiabi.FdflagNonblock != 0 {
		return n, err
	}
	w, ok := f.(Waitable)
	if !ok {
		return n, err
	}
	for {
		changed := w.Changed()
		n, err := op()
		if err != wasiabi.EAGAIN || n > 0 {
			return n, err
		}
		if err := c.p.Wait(c.ctx, time.Time{}, changed); err != wasiabi.ESUCCESS {
			return 0, err
		}
	}
}

// fdPread implements fd_pread(fd, iovs, iovs_len, offset, *nread).
func fdPread(c *call) wasiabi.Errno {
	d, bufs, err := c.descIovecs()
	if err != wasiabi.ESUCCESS {
		return err
	}
	off, err := offset(c.u64(3))
	if err != wasiabi.ESUCCESS {
		return err
	}
	n, err := transfer(bufs, func(p []byte, done int64) (int, wasiabi.Errno) {
		return d.file.Pread(p, off+done)
	})
	return c.putCount(4, n, err)
}

// fdPwrite implements fd_pwrite(fd, iovs, iovs_len, offset, *nwritten).
func fdPwrite(c *call) wasiabi.Errno {
	d, bufs, err := c.descIovecs()
	if err != wasiabi.ESUCCESS {
		return err
	}
	off, err := offset(c.u64(3))
	if err != wasiabi.ESUCCESS {
		return err
	}
	n, err := transfer(bufs, func(p []byte, done int64) (int, wasiabi.Errno) {
		return d.file.Pwrite(p, off+done)
	})
	return c.putCount(4, n, err)
}

func offset(off uint64) (int64, wasiabi.Errno) {
	if off > math.MaxInt64 {
		return 0, wasiabi.EINVAL
	}
	return int64(off), wasiabi.ESUCCESS
}

// descIovecs decodes the (fd, iovs, iovs_len) arguments shared by the
// read and write calls.
func (c *call) descIovecs() (*Desc, [][]byte, wasiabi.Errno) {
	d, err := c.desc(0)
	if err != wasiabi.ESUCCESS {
		return nil, nil, err
	}
	bufs, err := c.iovecs(c.u32(1), c.u32(2))
	return d, bufs, err
}

// transfer runs op on each buffer in turn, passing the bytes moved so far,
// and stops at the first short or failed transfer. An error after some
// bytes moved is dropped, as readv and writev do.
func transfer(bufs [][]byte, op func(p []byte, done int64) (int, wasiabi.Errno)) (int64, wasiabi.Errno) {
	var total int64
	for _, p := range bufs {
		if len(p) == 0 {
			continue
		}
		n, err := op(p, total)
		total += int64(n)
		if err != wasiabi.ESUCCESS {
			if total > 0 {
				return total, wasiabi.ESUCCESS
			}
			return 0, err
		}
		if n < len(p) {
			break
		}
	}
	return total, wasiabi.ESUCCESS
}

// putCount stores a transfer's byte count at the pointer in argument i.
func (c *call) putCount(i int, n int64, err wasiabi.Errno) wasiabi.Errno {
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.putU32(c.u32(i), uint32(n))
}

// fdPrestatGet implements fd_prestat_get(fd, *prestat), which succeeds
// only for preopens. Every other fd, open or not, answers EBADF, which
// ends wasi-libc's preopen scan.
func fdPrestatGet(c *call) wasiabi.Errno {
	name, err := c.preopen(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.put(c.u32(1), encodePrestat(name))
}

// fdPrestatDirName implements fd_prestat_dir_name(fd, path, path_len).
func fdPrestatDirName(c *call) wasiabi.Errno {
	name, err := c.preopen(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if c.u32(2) < uint32(len(name)) {
		return wasiabi.ENAMETOOLONG
	}
	return c.put(c.u32(1), []byte(name))
}

func (c *call) preopen(i int) (string, wasiabi.Errno) {
	d, err := c.desc(i)
	if err != wasiabi.ESUCCESS || d.Preopen() == "" {
		return "", wasiabi.EBADF
	}
	return d.Preopen(), wasiabi.ESUCCESS
}

// fdReaddir implements fd_readdir(fd, buf, buf_len, cookie, *bufused).
// The cookie is the index of the next entry in the directory's listing.
// See dirFile.entries. fdReaddir writes whole entries while they fit, then
// as much of the next as fits. A full buffer tells wasi-libc to retry with
// a bigger one.
func fdReaddir(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	dir, ok := f.(dirLister)
	if !ok {
		return wasiabi.ENOTDIR
	}
	buf, err := c.bytes(c.u32(1), c.u32(2))
	if err != wasiabi.ESUCCESS {
		return err
	}
	cookie := c.u64(3)
	ents, err := dir.entries(cookie)
	if err != wasiabi.ESUCCESS {
		return err
	}
	used := 0
	for i, e := range ents {
		if used == len(buf) {
			break
		}
		used += copy(buf[used:], encodeDirent(e, cookie+uint64(i)+1))
	}
	return c.putU32(c.u32(4), uint32(used))
}

// fdRenumber implements fd_renumber(fd, to).
func fdRenumber(c *call) wasiabi.Errno { return c.p.FDs.Renumber(c.fd(0), c.fd(1)) }

// fdSeek implements fd_seek(fd, offset, whence, *newoffset).
func fdSeek(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	whence := c.u32(2)
	if whence > uint32(wasiabi.WhenceEnd) {
		return wasiabi.EINVAL
	}
	off, err := f.Lseek(int64(c.u64(1)), uint8(whence))
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.putU64(c.u32(3), uint64(off))
}

// fdTell implements fd_tell(fd, *offset).
func fdTell(c *call) wasiabi.Errno {
	f, err := c.file(0)
	if err != wasiabi.ESUCCESS {
		return err
	}
	off, err := f.Lseek(0, wasiabi.WhenceCur)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.putU64(c.u32(1), uint64(off))
}

// pathCreateDirectory implements path_create_directory(fd, path, path_len).
func pathCreateDirectory(c *call) wasiabi.Errno {
	dir, path, err := c.dirPath(0, 1)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.p.FS.Mkdir(dir, path)
}

// pathFilestatGet implements path_filestat_get(fd, lookupflags, path,
// path_len, *filestat).
func pathFilestatGet(c *call) wasiabi.Errno {
	n, err := c.lookup()
	if err != wasiabi.ESUCCESS {
		return err
	}
	st := c.p.FS.Stat(n)
	return c.put(c.u32(4), encodeFilestat(st, filetypeOf(st.Kind)))
}

// pathFilestatSetTimes implements path_filestat_set_times(fd, lookupflags,
// path, path_len, atim, mtim, fstflags).
func pathFilestatSetTimes(c *call) wasiabi.Errno {
	atim, mtim, err := times(c.u64(4), c.u64(5), uint16(c.u32(6)))
	if err != wasiabi.ESUCCESS {
		return err
	}
	n, err := c.lookup()
	if err != wasiabi.ESUCCESS {
		return err
	}
	c.p.FS.SetTimes(n, atim, mtim)
	return wasiabi.ESUCCESS
}

// lookup resolves the (fd, lookupflags, path, path_len) arguments.
func (c *call) lookup() (*memfs.Node, wasiabi.Errno) {
	dir, path, err := c.dirPath(0, 2)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	return c.p.FS.Lookup(dir, path, c.u32(1)&wasiabi.LookupSymlinkFollow != 0)
}

// pathLink implements path_link(old_fd, old_flags, old_path, old_path_len,
// new_fd, new_path, new_path_len).
func pathLink(c *call) wasiabi.Errno {
	oldDir, oldPath, err := c.dirPath(0, 2)
	if err != wasiabi.ESUCCESS {
		return err
	}
	newDir, newPath, err := c.dirPath(4, 5)
	if err != wasiabi.ESUCCESS {
		return err
	}
	follow := c.u32(1)&wasiabi.LookupSymlinkFollow != 0
	return c.p.FS.Link(oldDir, oldPath, newDir, newPath, follow)
}

// pathOpen implements path_open(fd, dirflags, path, path_len, oflags,
// rights_base, rights_inheriting, fdflags, *fd). Of the rights, only
// fd_write matters. Asking for it on a directory gives EISDIR.
func pathOpen(c *call) wasiabi.Errno {
	dir, path, err := c.dirPath(0, 2)
	if err != wasiabi.ESUCCESS {
		return err
	}
	oflags := uint16(c.u32(4))
	n, err := c.p.FS.Open(dir, path, memfs.OpenFlags{
		Create:    oflags&wasiabi.OflagCreat != 0,
		Excl:      oflags&wasiabi.OflagExcl != 0,
		Trunc:     oflags&wasiabi.OflagTrunc != 0,
		Directory: oflags&wasiabi.OflagDirectory != 0,
		NoFollow:  c.u32(1)&wasiabi.LookupSymlinkFollow == 0,
	})
	if err != wasiabi.ESUCCESS {
		return err
	}
	f := newNodeFile(c.p.FS, n, c.p.Random)
	if f.DirNode() != nil && c.u64(5)&rightFdWrite != 0 {
		return wasiabi.EISDIR
	}
	fd, err := c.p.FDs.Install(NewDesc(f, uint16(c.u32(7))))
	if err != wasiabi.ESUCCESS {
		return err
	}
	if err := c.putU32(c.u32(8), uint32(fd)); err != wasiabi.ESUCCESS {
		c.p.FDs.Close(fd)
		return err
	}
	return wasiabi.ESUCCESS
}

// pathReadlink implements path_readlink(fd, path, path_len, buf, buf_len,
// *bufused). It truncates the target to the buffer, as readlink does.
func pathReadlink(c *call) wasiabi.Errno {
	dir, path, err := c.dirPath(0, 1)
	if err != wasiabi.ESUCCESS {
		return err
	}
	target, err := c.p.FS.Readlink(dir, path)
	if err != wasiabi.ESUCCESS {
		return err
	}
	buf, err := c.bytes(c.u32(3), c.u32(4))
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.putU32(c.u32(5), uint32(copy(buf, target)))
}

// pathRemoveDirectory implements path_remove_directory(fd, path, path_len).
func pathRemoveDirectory(c *call) wasiabi.Errno {
	dir, path, err := c.dirPath(0, 1)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.p.FS.Rmdir(dir, path)
}

// pathRename implements path_rename(fd, old_path, old_path_len, new_fd,
// new_path, new_path_len).
func pathRename(c *call) wasiabi.Errno {
	oldDir, oldPath, err := c.dirPath(0, 1)
	if err != wasiabi.ESUCCESS {
		return err
	}
	newDir, newPath, err := c.dirPath(3, 4)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.p.FS.Rename(oldDir, oldPath, newDir, newPath)
}

// pathSymlink implements path_symlink(old_path, old_path_len, fd, new_path,
// new_path_len).
func pathSymlink(c *call) wasiabi.Errno {
	target, err := c.path(0) // memfs refuses targets over MaxSymlinkLen
	if err != wasiabi.ESUCCESS {
		return err
	}
	dir, path, err := c.dirPath(2, 3)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.p.FS.Symlink(target, dir, path)
}

// pathUnlinkFile implements path_unlink_file(fd, path, path_len).
func pathUnlinkFile(c *call) wasiabi.Errno {
	dir, path, err := c.dirPath(0, 1)
	if err != wasiabi.ESUCCESS {
		return err
	}
	return c.p.FS.Unlink(dir, path)
}
