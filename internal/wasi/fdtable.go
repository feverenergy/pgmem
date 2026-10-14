package wasi

import (
	"sync"
	"sync/atomic"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// File is an open file object. Methods a kind does not support return
// what POSIX would: EBADF for the wrong direction, ESPIPE for no offset,
// EISDIR or ENOTDIR. Implementations embed BaseFile for those defaults.
type File interface {
	Filetype() uint8
	Read(p []byte, flags uint16) (int, wasiabi.Errno) // flags are the Desc's fdflags
	Write(p []byte, flags uint16) (int, wasiabi.Errno)
	Pread(p []byte, off int64) (int, wasiabi.Errno)
	Pwrite(p []byte, off int64) (int, wasiabi.Errno)
	Lseek(off int64, whence uint8) (int64, wasiabi.Errno)
	Stat() (memfs.Stat, wasiabi.Errno)
	SetSize(size int64) wasiabi.Errno
	SetTimes(atim, mtim *int64) wasiabi.Errno
	Sync() wasiabi.Errno
	DirNode() *memfs.Node // non-nil only for directories
	// Ready reports poll readiness for EventtypeFdRead or EventtypeFdWrite.
	Ready(event uint8) (ready bool, nbytes uint64, hangup bool)
	Close() wasiabi.Errno
}

// BaseFile supplies the "not supported" answers. Embed it.
type BaseFile struct{}

// Filetype reports an unknown file type.
func (BaseFile) Filetype() uint8 { return wasiabi.FiletypeUnknown }

// Read fails, because the file is not open for reading.
func (BaseFile) Read([]byte, uint16) (int, wasiabi.Errno) { return 0, wasiabi.EBADF }

// Write fails, because the file is not open for writing.
func (BaseFile) Write([]byte, uint16) (int, wasiabi.Errno) { return 0, wasiabi.EBADF }

// Pread fails, because the file has no offset.
func (BaseFile) Pread([]byte, int64) (int, wasiabi.Errno) { return 0, wasiabi.ESPIPE }

// Pwrite fails, because the file has no offset.
func (BaseFile) Pwrite([]byte, int64) (int, wasiabi.Errno) { return 0, wasiabi.ESPIPE }

// Lseek fails, because the file has no offset.
func (BaseFile) Lseek(int64, uint8) (int64, wasiabi.Errno) { return 0, wasiabi.ESPIPE }

// Stat reports a nameless object with one link.
func (BaseFile) Stat() (memfs.Stat, wasiabi.Errno) {
	return memfs.Stat{Nlink: 1}, wasiabi.ESUCCESS
}

// SetSize fails, because only regular files have a size to set.
func (BaseFile) SetSize(int64) wasiabi.Errno { return wasiabi.EINVAL }

// SetTimes is accepted and ignored.
func (BaseFile) SetTimes(_, _ *int64) wasiabi.Errno { return wasiabi.ESUCCESS }

// Sync fails as fsync does on a pipe or terminal.
func (BaseFile) Sync() wasiabi.Errno { return wasiabi.EINVAL }

// DirNode returns nil, because the file is not a directory.
func (BaseFile) DirNode() *memfs.Node { return nil }

// Ready reports the file always ready, as POSIX does for regular files.
func (BaseFile) Ready(uint8) (bool, uint64, bool) { return true, 0, false }

// Close does nothing.
func (BaseFile) Close() wasiabi.Errno { return wasiabi.ESUCCESS }

// Desc is an open file description, the thing dup'ed fds share. The
// offset lives in the File, and the flags live here. It is
// reference-counted, and the File closes when the last fd referring to it
// closes.
type Desc struct {
	file    File
	mu      sync.Mutex // guards flags
	flags   uint16
	refs    atomic.Int32
	preopen string
}

// NewDesc returns a description of f holding one reference.
func NewDesc(f File, flags uint16) *Desc {
	d := &Desc{file: f, flags: flags}
	d.refs.Store(1)
	return d
}

// File returns the open file.
func (d *Desc) File() File { return d.file }

// Flags returns the fdflags.
func (d *Desc) Flags() uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.flags
}

// SetFlags replaces the fdflags.
func (d *Desc) SetFlags(f uint16) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flags = f
}

// Preopen returns the preopened directory's name, or "" for other files.
func (d *Desc) Preopen() string { return d.preopen }

func (d *Desc) retain() { d.refs.Add(1) }

// release drops one reference and closes the file with the last one.
func (d *Desc) release() wasiabi.Errno {
	if d.refs.Add(-1) == 0 {
		return d.file.Close()
	}
	return wasiabi.ESUCCESS
}

// MaxFiles is the fd ceiling, and every fd is below it. It matches the
// RLIMIT_NOFILE the compat layer reports, which is PGMEM_MAX_FILES in
// postgres/compat/src/resource.c.
const MaxFiles = 4096

// FDTable maps fd numbers to descriptions. It is safe for concurrent use.
//
// Close-on-exec is a flag of the fd, not of the description. Dup'ed fds
// share a description, but each has its own flag. Every way to create an
// fd clears the flag, with two exceptions. DupFrom sets it when asked to,
// and Renumber moves an fd with its flag.
type FDTable struct {
	mu  sync.Mutex
	fds map[int32]slot
}

// slot is one open fd.
type slot struct {
	d       *Desc
	cloexec bool
}

// NewFDTable returns an empty table.
func NewFDTable() *FDTable {
	return &FDTable{fds: map[int32]slot{}}
}

// lowestFree returns the lowest fd ≥ min that is not open, or false if
// every fd from min up to MaxFiles is. The caller holds t.mu.
func (t *FDTable) lowestFree(min int32) (int32, bool) {
	for fd := min; fd < MaxFiles; fd++ {
		if _, ok := t.fds[fd]; !ok {
			return fd, true
		}
	}
	return -1, false
}

// Install puts d at the lowest free fd ≥ 0 and returns it. The table takes
// the caller's reference. If every fd below MaxFiles is open, it releases
// the reference and fails with EMFILE.
func (t *FDTable) Install(d *Desc) (int32, wasiabi.Errno) {
	t.mu.Lock()
	fd, ok := t.lowestFree(0)
	if ok {
		t.fds[fd] = slot{d: d}
	}
	t.mu.Unlock()
	if !ok {
		d.release()
		return -1, wasiabi.EMFILE
	}
	return fd, wasiabi.ESUCCESS
}

// InstallAt puts d at fd, releasing whatever was there. The table takes the
// caller's reference. The fd is not close-on-exec. fd must be in [0,
// MaxFiles).
func (t *FDTable) InstallAt(fd int32, d *Desc) {
	t.mu.Lock()
	old := t.fds[fd]
	t.fds[fd] = slot{d: d}
	t.mu.Unlock()
	if old.d != nil {
		old.d.release()
	}
}

// Get returns the description at fd, or EBADF if fd is not open.
func (t *FDTable) Get(fd int32) (*Desc, wasiabi.Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.fds[fd]
	if !ok {
		return nil, wasiabi.EBADF
	}
	return s.d, wasiabi.ESUCCESS
}

// Dup returns the description at fd with a new reference, for installing in
// another table.
func (t *FDTable) Dup(fd int32) (*Desc, wasiabi.Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.fds[fd]
	if !ok {
		return nil, wasiabi.EBADF
	}
	s.d.retain()
	return s.d, wasiabi.ESUCCESS
}

// DupFrom installs a new reference to fd's description at the lowest free
// fd ≥ min, as fcntl(F_DUPFD) does. cloexec sets the new fd's flag, as
// F_DUPFD_CLOEXEC does. A min that is negative or not below MaxFiles fails
// with EINVAL. If no fd from min up to MaxFiles is free, DupFrom fails
// with EMFILE.
func (t *FDTable) DupFrom(fd, min int32, cloexec bool) (int32, wasiabi.Errno) {
	if min < 0 || min >= MaxFiles {
		return -1, wasiabi.EINVAL
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.fds[fd]
	if !ok {
		return -1, wasiabi.EBADF
	}
	nfd, ok := t.lowestFree(min)
	if !ok {
		return -1, wasiabi.EMFILE
	}
	s.d.retain()
	t.fds[nfd] = slot{d: s.d, cloexec: cloexec}
	return nfd, wasiabi.ESUCCESS
}

// Dup2 makes newfd refer to fd's description and closes what newfd held.
// dup2(fd, fd) is a no-op that only checks fd is open. The new fd is not
// close-on-exec. A newfd that is negative or not below MaxFiles fails with
// EBADF.
func (t *FDTable) Dup2(fd, newfd int32) wasiabi.Errno {
	if newfd < 0 || newfd >= MaxFiles {
		return wasiabi.EBADF
	}
	t.mu.Lock()
	s, ok := t.fds[fd]
	if !ok {
		t.mu.Unlock()
		return wasiabi.EBADF
	}
	if fd == newfd {
		t.mu.Unlock()
		return wasiabi.ESUCCESS
	}
	s.d.retain()
	old := t.fds[newfd]
	t.fds[newfd] = slot{d: s.d}
	t.mu.Unlock()
	if old.d != nil {
		old.d.release()
	}
	return wasiabi.ESUCCESS
}

// Cloexec reports whether fd is close-on-exec, or EBADF.
func (t *FDTable) Cloexec(fd int32) (bool, wasiabi.Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.fds[fd]
	if !ok {
		return false, wasiabi.EBADF
	}
	return s.cloexec, wasiabi.ESUCCESS
}

// SetCloexec sets or clears fd's close-on-exec flag, or fails with EBADF.
func (t *FDTable) SetCloexec(fd int32, on bool) wasiabi.Errno {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.fds[fd]
	if !ok {
		return wasiabi.EBADF
	}
	s.cloexec = on
	t.fds[fd] = s
	return wasiabi.ESUCCESS
}

// InheritInto copies t's fds that are not close-on-exec into child. For
// each, it installs a new reference to the description at the same
// number, not close-on-exec, and releases whatever child held there. It
// skips fd 3, the preopen, because the child has its own. t and child must
// differ.
func (t *FDTable) InheritInto(child *FDTable) {
	t.mu.Lock()
	inherit := make(map[int32]*Desc, len(t.fds))
	for fd, s := range t.fds {
		if fd == preopenFD || s.cloexec {
			continue
		}
		s.d.retain()
		inherit[fd] = s.d
	}
	t.mu.Unlock()
	for fd, d := range inherit {
		child.InstallAt(fd, d)
	}
}

// Close removes fd and releases its description.
func (t *FDTable) Close(fd int32) wasiabi.Errno {
	t.mu.Lock()
	s, ok := t.fds[fd]
	delete(t.fds, fd)
	t.mu.Unlock()
	if !ok {
		return wasiabi.EBADF
	}
	return s.d.release()
}

// Renumber moves the fd at from to to and closes what was at to. It acts
// as fd_renumber does, and as dup2 followed by close does. from must be
// open, and to must be in [0, MaxFiles). Otherwise Renumber fails with
// EBADF. The fd keeps its close-on-exec flag, because it is moved, not
// duplicated.
func (t *FDTable) Renumber(from, to int32) wasiabi.Errno {
	if to < 0 || to >= MaxFiles {
		return wasiabi.EBADF
	}
	t.mu.Lock()
	s, ok := t.fds[from]
	if !ok {
		t.mu.Unlock()
		return wasiabi.EBADF
	}
	if from == to {
		t.mu.Unlock()
		return wasiabi.ESUCCESS
	}
	old := t.fds[to]
	t.fds[to] = s
	delete(t.fds, from)
	t.mu.Unlock()
	if old.d != nil {
		old.d.release()
	}
	return wasiabi.ESUCCESS
}

// CloseAll closes every fd.
func (t *FDTable) CloseAll() {
	t.mu.Lock()
	fds := t.fds
	t.fds = map[int32]slot{}
	t.mu.Unlock()
	for _, s := range fds {
		s.d.release()
	}
}

// Len returns the number of open fds.
func (t *FDTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.fds)
}
