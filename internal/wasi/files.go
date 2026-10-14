package wasi

import (
	"errors"
	"io"
	"math"
	"sync"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// NewNodeFile returns the File for a memfs node. That is a regular file
// with its own offset, a directory, or a device.
func NewNodeFile(fs *memfs.FS, n *memfs.Node) File { return newNodeFile(fs, n, nil) }

// newNodeFile is NewNodeFile where /dev/urandom reads from random, unless
// it is nil.
func newNodeFile(fs *memfs.FS, n *memfs.Node, random io.Reader) File {
	node := nodeFile{fs: fs, n: n}
	switch fs.Stat(n).Kind {
	case memfs.KindDir:
		return &dirFile{nodeFile: node}
	case memfs.KindDevice:
		if !fs.IsURandom(n) {
			random = nil
		}
		return &deviceFile{nodeFile: node, random: random}
	}
	return &regularFile{nodeFile: node}
}

// filetypeOf maps a memfs kind to its WASI filetype.
func filetypeOf(k memfs.Kind) uint8 {
	switch k {
	case memfs.KindDir:
		return wasiabi.FiletypeDirectory
	case memfs.KindFile:
		return wasiabi.FiletypeRegularFile
	case memfs.KindSymlink:
		return wasiabi.FiletypeSymbolicLink
	case memfs.KindDevice:
		return wasiabi.FiletypeCharacterDevice
	}
	return wasiabi.FiletypeUnknown
}

// nodeFile is what every memfs-backed file shares.
type nodeFile struct {
	BaseFile
	fs *memfs.FS
	n  *memfs.Node
}

func (f *nodeFile) Stat() (memfs.Stat, wasiabi.Errno) { return f.fs.Stat(f.n), wasiabi.ESUCCESS }

func (f *nodeFile) SetTimes(atim, mtim *int64) wasiabi.Errno {
	f.fs.SetTimes(f.n, atim, mtim)
	return wasiabi.ESUCCESS
}

// regularFile is an open regular file with its own offset.
type regularFile struct {
	nodeFile
	mu  sync.Mutex // guards off
	off int64
}

func (f *regularFile) Filetype() uint8 { return filetypeOf(f.fs.Stat(f.n).Kind) }

func (f *regularFile) Read(p []byte, _ uint16) (int, wasiabi.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.fs.ReadAt(f.n, p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *regularFile) Write(p []byte, flags uint16) (int, wasiabi.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flags&wasiabi.FdflagAppend != 0 {
		f.off = f.fs.Stat(f.n).Size
	}
	n, err := f.fs.WriteAt(f.n, p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *regularFile) Pread(p []byte, off int64) (int, wasiabi.Errno) {
	return f.fs.ReadAt(f.n, p, off)
}

func (f *regularFile) Pwrite(p []byte, off int64) (int, wasiabi.Errno) {
	return f.fs.WriteAt(f.n, p, off)
}

func (f *regularFile) Lseek(off int64, whence uint8) (int64, wasiabi.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var base int64
	switch whence {
	case wasiabi.WhenceSet:
	case wasiabi.WhenceCur:
		base = f.off
	case wasiabi.WhenceEnd:
		base = f.fs.Stat(f.n).Size
	default:
		return 0, wasiabi.EINVAL
	}
	if off > 0 && base > math.MaxInt64-off {
		return 0, wasiabi.EOVERFLOW
	}
	if base+off < 0 {
		return 0, wasiabi.EINVAL
	}
	f.off = base + off
	return f.off, wasiabi.ESUCCESS
}

func (f *regularFile) SetSize(size int64) wasiabi.Errno { return f.fs.Truncate(f.n, size) }

func (f *regularFile) Sync() wasiabi.Errno { return wasiabi.ESUCCESS }

func (f *regularFile) Ready(event uint8) (bool, uint64, bool) {
	if event != wasiabi.EventtypeFdRead {
		return true, 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return true, uint64(max(f.fs.Stat(f.n).Size-f.off, 0)), false
}

// dirFile is an open directory.
type dirFile struct {
	nodeFile
	mu      sync.Mutex     // guards listing
	listing []memfs.Dirent // the snapshot fd_readdir's cookies index
}

// dirLister is what fd_readdir reads.
type dirLister interface {
	entries(cookie uint64) ([]memfs.Dirent, wasiabi.Errno)
}

// entries returns the directory's listing from index cookie on. The
// listing is ".", "..", then the names in order. Cookie 0, from opendir or
// rewinddir, takes a fresh snapshot, and later cookies index into it. So a
// program that unlinks entries while it reads sees every entry present at
// the start exactly once. Postgres's rmtree does that.
func (f *dirFile) entries(cookie uint64) ([]memfs.Dirent, wasiabi.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cookie == 0 || f.listing == nil {
		ents, err := listDir(f.fs, f.n)
		if err != wasiabi.ESUCCESS {
			return nil, err
		}
		f.listing = ents
	}
	if cookie >= uint64(len(f.listing)) {
		return nil, wasiabi.ESUCCESS
	}
	return f.listing[cookie:], wasiabi.ESUCCESS // a new snapshot replaces, never mutates, this one
}

// listDir lists dir with "." and ".." first.
func listDir(fs *memfs.FS, dir *memfs.Node) ([]memfs.Dirent, wasiabi.Errno) {
	names, err := fs.Readdir(dir)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	parent, err := fs.Lookup(dir, "..", false) // the root is its own parent
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	dots := []memfs.Dirent{
		{Name: ".", Ino: fs.Stat(dir).Ino, Kind: memfs.KindDir},
		{Name: "..", Ino: fs.Stat(parent).Ino, Kind: memfs.KindDir},
	}
	return append(dots, names...), wasiabi.ESUCCESS
}

func (f *dirFile) Filetype() uint8                           { return wasiabi.FiletypeDirectory }
func (f *dirFile) Read([]byte, uint16) (int, wasiabi.Errno)  { return 0, wasiabi.EISDIR }
func (f *dirFile) Write([]byte, uint16) (int, wasiabi.Errno) { return 0, wasiabi.EISDIR }
func (f *dirFile) Pread([]byte, int64) (int, wasiabi.Errno)  { return 0, wasiabi.EISDIR }
func (f *dirFile) Pwrite([]byte, int64) (int, wasiabi.Errno) { return 0, wasiabi.EISDIR }
func (f *dirFile) Lseek(int64, uint8) (int64, wasiabi.Errno) { return 0, wasiabi.EISDIR }
func (f *dirFile) SetSize(int64) wasiabi.Errno               { return wasiabi.EISDIR }
func (f *dirFile) Sync() wasiabi.Errno                       { return wasiabi.ESUCCESS }
func (f *dirFile) DirNode() *memfs.Node                      { return f.n }

// deviceFile is an open /dev node. It has no offset, and seeks answer 0,
// as Linux does for /dev/null.
type deviceFile struct {
	nodeFile
	random io.Reader // /dev/urandom's bytes instead of memfs's; nil means memfs's
}

func (f *deviceFile) Filetype() uint8 { return wasiabi.FiletypeCharacterDevice }

func (f *deviceFile) Read(p []byte, _ uint16) (int, wasiabi.Errno) {
	return f.Pread(p, 0)
}

func (f *deviceFile) Write(p []byte, _ uint16) (int, wasiabi.Errno) {
	return f.fs.WriteAt(f.n, p, 0)
}

func (f *deviceFile) Pread(p []byte, off int64) (int, wasiabi.Errno) {
	if f.random != nil {
		if _, err := io.ReadFull(f.random, p); err != nil {
			return 0, wasiabi.EIO
		}
		return len(p), wasiabi.ESUCCESS
	}
	return f.fs.ReadAt(f.n, p, off)
}

func (f *deviceFile) Pwrite(p []byte, off int64) (int, wasiabi.Errno) {
	return f.fs.WriteAt(f.n, p, off)
}

func (f *deviceFile) Lseek(int64, uint8) (int64, wasiabi.Errno) { return 0, wasiabi.ESUCCESS }

// hostStream is a character device backed by a Go stream, the stdin,
// stdout or stderr of a process started from Go. It is not seekable, and
// fd_fdstat_get reports it without seek rights so that isatty sees a
// terminal.
type hostStream interface {
	File
	hostStream()
}

type streamFile struct{ BaseFile }

func (streamFile) hostStream()     {}
func (streamFile) Filetype() uint8 { return wasiabi.FiletypeCharacterDevice }

func (streamFile) Stat() (memfs.Stat, wasiabi.Errno) {
	return memfs.Stat{Kind: memfs.KindDevice, Nlink: 1}, wasiabi.ESUCCESS
}

// readStream reads from an io.Reader. A nil one is at EOF.
type readStream struct {
	streamFile
	mu sync.Mutex
	r  io.Reader
}

func (s *readStream) Read(p []byte, _ uint16) (int, wasiabi.Errno) {
	if s.r == nil || len(p) == 0 {
		return 0, wasiabi.ESUCCESS
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && n == 0 {
		return 0, wasiabi.EIO
	}
	return n, wasiabi.ESUCCESS
}

// writeStream writes to an io.Writer. A nil one discards.
type writeStream struct {
	streamFile
	mu sync.Mutex
	w  io.Writer
}

func (s *writeStream) Write(p []byte, _ uint16) (int, wasiabi.Errno) {
	if s.w == nil {
		return len(p), wasiabi.ESUCCESS
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.w.Write(p)
	if err != nil && n == 0 {
		return 0, wasiabi.EIO
	}
	return n, wasiabi.ESUCCESS
}
