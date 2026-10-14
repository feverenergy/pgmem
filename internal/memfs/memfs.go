// Package memfs is pgmem's in-memory file tree, shared by every process.
// Errors are WASI errno values. All methods are safe for concurrent use.
package memfs

import (
	"crypto/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// Kind is the type of a Node.
type Kind uint8

// Node kinds.
const (
	KindDir Kind = iota + 1
	KindFile
	KindSymlink
	KindDevice
)

// MaxFileSize caps a regular file. Writing past it fails with EFBIG.
// Postgres never needs a single file over 1 GiB. Relation and temp segments
// are 1 GiB, and WAL segments are 16 MiB.
const MaxFileSize int64 = 1 << 31

type device uint8

const (
	devNone device = iota
	devNull
	devZero
	devURandom
)

// Node is a file, directory, symlink or device. Its fields are private, so
// use FS methods. A Node stays valid after its last name is removed.
type Node struct {
	// ino, kind and dev never change. FS.mu guards everything else.
	ino  uint64
	kind Kind
	dev  device

	nlink            uint64
	atim, mtim, ctim int64

	data     []byte           // KindFile
	children map[string]*Node // KindDir
	parent   *Node            // KindDir; the root is its own parent
	target   string           // KindSymlink
}

// Stat describes a node. Times are nanoseconds since the Unix epoch.
type Stat struct {
	Ino              uint64
	Kind             Kind
	Nlink            uint64
	Size             int64
	Atim, Mtim, Ctim int64
}

// Dirent is one directory entry. It is never "." or "..".
type Dirent struct {
	Name string
	Ino  uint64
	Kind Kind
}

// OpenFlags select Open's behaviour.
type OpenFlags struct {
	Create    bool // create a regular file if missing
	Excl      bool // with Create, fail with EEXIST if it exists
	Trunc     bool // truncate a regular file to 0
	Directory bool // fail with ENOTDIR unless the result is a directory
	NoFollow  bool // fail with ELOOP if the last component is a symlink
}

// FS is an in-memory file tree. One RWMutex guards the whole tree and all
// file data. Read-only operations take the read lock, and everything else
// takes the write lock.
type FS struct {
	mu      sync.RWMutex
	root    *Node
	lastIno uint64 // inode numbers are never reused
	clock   int64  // last time handed out by now
}

// New returns a tree holding the root plus /dev/null, /dev/zero and
// /dev/urandom.
func New() *FS {
	fs := &FS{}
	fs.root = fs.newNode(KindDir)
	fs.root.parent = fs.root
	fs.root.nlink = 1
	dev := fs.newNode(KindDir)
	fs.addEntry(fs.root, "dev", dev)
	for _, d := range []struct {
		name string
		dev  device
	}{{"null", devNull}, {"zero", devZero}, {"urandom", devURandom}} {
		n := fs.newNode(KindDevice)
		n.dev = d.dev
		fs.addEntry(dev, d.name, n)
	}
	return fs
}

// Root returns the root directory.
func (fs *FS) Root() *Node { return fs.root }

// Lookup resolves path from dir, or from the root if path is absolute. It
// follows a symlink in the last component only if follow is set.
func (fs *FS) Lookup(dir *Node, path string, follow bool) (*Node, wasiabi.Errno) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	w := walker{fs: fs}
	return w.lookup(dir, path, follow)
}

// Open resolves path and applies f, creating a regular file if asked.
func (fs *FS) Open(dir *Node, path string, f OpenFlags) (*Node, wasiabi.Errno) {
	if f.Create || f.Trunc {
		fs.mu.Lock()
		defer fs.mu.Unlock()
	} else {
		fs.mu.RLock()
		defer fs.mu.RUnlock()
	}
	return fs.open(dir, path, f)
}

func (fs *FS) open(dir *Node, path string, f OpenFlags) (*Node, wasiabi.Errno) {
	w := walker{fs: fs}
	t, err := w.parent(dir, path)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	n := child(t.dir, t.name)
	switch {
	case n == nil && !f.Create:
		return nil, wasiabi.ENOENT
	case n == nil && t.mustDir:
		return nil, wasiabi.EISDIR
	case n == nil:
		if t.dir.nlink == 0 {
			return nil, wasiabi.ENOENT // the directory was removed
		}
		n = fs.newNode(KindFile)
		fs.addEntry(t.dir, t.name, n)
		return n, wasiabi.ESUCCESS
	case f.Create && f.Excl:
		return nil, wasiabi.EEXIST
	}
	if n.kind == KindSymlink {
		if f.NoFollow && !t.mustDir {
			return nil, wasiabi.ELOOP
		}
		if n, err = w.follow(t.dir, n); err != wasiabi.ESUCCESS {
			return nil, err
		}
	}
	if (f.Directory || t.mustDir) && n.kind != KindDir {
		return nil, wasiabi.ENOTDIR
	}
	if f.Trunc {
		switch n.kind {
		case KindDir:
			return nil, wasiabi.EISDIR
		case KindFile:
			fs.truncate(n, 0)
		}
	}
	return n, wasiabi.ESUCCESS
}

// Mkdir creates a directory.
func (fs *FS) Mkdir(dir *Node, path string) wasiabi.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.mkdir(dir, path)
}

func (fs *FS) mkdir(dir *Node, path string) wasiabi.Errno {
	w := walker{fs: fs}
	t, err := w.parent(dir, path)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if child(t.dir, t.name) != nil {
		return wasiabi.EEXIST
	}
	if t.dir.nlink == 0 {
		return wasiabi.ENOENT
	}
	fs.addEntry(t.dir, t.name, fs.newNode(KindDir))
	return wasiabi.ESUCCESS
}

// Rmdir removes an empty directory.
func (fs *FS) Rmdir(dir *Node, path string) wasiabi.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	w := walker{fs: fs}
	t, err := w.parent(dir, path)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if isDot(t.name) {
		return wasiabi.EINVAL
	}
	n := t.dir.children[t.name]
	switch {
	case n == nil:
		return wasiabi.ENOENT
	case n.kind != KindDir:
		return wasiabi.ENOTDIR
	case len(n.children) > 0:
		return wasiabi.ENOTEMPTY
	}
	fs.removeEntry(t.dir, t.name, n)
	return wasiabi.ESUCCESS
}

// Unlink removes a name of a non-directory.
func (fs *FS) Unlink(dir *Node, path string) wasiabi.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	w := walker{fs: fs}
	t, err := w.parent(dir, path)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if isDot(t.name) {
		return wasiabi.EISDIR
	}
	n := t.dir.children[t.name]
	switch {
	case n == nil:
		return wasiabi.ENOENT
	case n.kind == KindDir:
		return wasiabi.EISDIR
	case t.mustDir:
		return wasiabi.ENOTDIR
	}
	fs.removeEntry(t.dir, t.name, n)
	return wasiabi.ESUCCESS
}

// Rename moves oldPath to newPath, replacing a file or an empty directory
// there. It follows neither last component.
func (fs *FS) Rename(oldDir *Node, oldPath string, newDir *Node, newPath string) wasiabi.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	w := walker{fs: fs}
	from, err := w.parent(oldDir, oldPath)
	if err != wasiabi.ESUCCESS {
		return err
	}
	to, err := w.parent(newDir, newPath)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if isDot(from.name) || isDot(to.name) {
		return wasiabi.EINVAL
	}
	src := from.dir.children[from.name]
	if src == nil {
		return wasiabi.ENOENT
	}
	if (from.mustDir || to.mustDir) && src.kind != KindDir {
		return wasiabi.ENOTDIR
	}
	dst := to.dir.children[to.name]
	if dst == src {
		return wasiabi.ESUCCESS
	}
	if src.kind == KindDir {
		for d := to.dir; ; d = d.parent {
			if d == src {
				return wasiabi.EINVAL // into its own subtree
			}
			if d == fs.root {
				break
			}
		}
	}
	if dst != nil {
		switch {
		case src.kind == KindDir && dst.kind != KindDir:
			return wasiabi.ENOTDIR
		case src.kind != KindDir && dst.kind == KindDir:
			return wasiabi.EISDIR
		case dst.kind == KindDir && len(dst.children) > 0:
			return wasiabi.ENOTEMPTY
		}
	}
	if to.dir.nlink == 0 {
		return wasiabi.ENOENT
	}
	fs.removeEntry(from.dir, from.name, src)
	if dst != nil {
		fs.removeEntry(to.dir, to.name, dst)
	}
	fs.addEntry(to.dir, to.name, src)
	return wasiabi.ESUCCESS
}

// Link gives the node at oldPath the new name newPath. follow says whether
// to follow a symlink in oldPath's last component.
func (fs *FS) Link(oldDir *Node, oldPath string, newDir *Node, newPath string, follow bool) wasiabi.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.link(oldDir, oldPath, newDir, newPath, follow)
}

func (fs *FS) link(oldDir *Node, oldPath string, newDir *Node, newPath string, follow bool) wasiabi.Errno {
	w := walker{fs: fs}
	src, err := w.lookup(oldDir, oldPath, follow)
	if err != wasiabi.ESUCCESS {
		return err
	}
	if src.kind == KindDir {
		return wasiabi.EPERM
	}
	return fs.addNew(newDir, newPath, func() *Node { return src })
}

// Symlink creates a symlink at path holding target, stored as-is. A target
// over MaxSymlinkLen fails with ENAMETOOLONG.
func (fs *FS) Symlink(target string, dir *Node, path string) wasiabi.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.symlink(target, dir, path)
}

func (fs *FS) symlink(target string, dir *Node, path string) wasiabi.Errno {
	if len(target) > MaxSymlinkLen {
		return wasiabi.ENAMETOOLONG
	}
	return fs.addNew(dir, path, func() *Node {
		n := fs.newNode(KindSymlink)
		n.target = target
		return n
	})
}

// addNew enters the node made by mk under path, which must not exist yet.
func (fs *FS) addNew(dir *Node, path string, mk func() *Node) wasiabi.Errno {
	w := walker{fs: fs}
	t, err := w.parent(dir, path)
	if err != wasiabi.ESUCCESS {
		return err
	}
	switch {
	case child(t.dir, t.name) != nil:
		return wasiabi.EEXIST
	case t.mustDir:
		return wasiabi.ENOENT
	case t.dir.nlink == 0:
		return wasiabi.ENOENT
	}
	fs.addEntry(t.dir, t.name, mk())
	return wasiabi.ESUCCESS
}

// Readlink returns the target of the symlink at path.
func (fs *FS) Readlink(dir *Node, path string) (string, wasiabi.Errno) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	w := walker{fs: fs}
	n, err := w.lookup(dir, path, false)
	if err != wasiabi.ESUCCESS {
		return "", err
	}
	if n.kind != KindSymlink {
		return "", wasiabi.EINVAL
	}
	return n.target, wasiabi.ESUCCESS
}

// Stat describes n.
func (fs *FS) Stat(n *Node) Stat {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	st := Stat{Ino: n.ino, Kind: n.kind, Nlink: n.nlink, Atim: n.atim, Mtim: n.mtim, Ctim: n.ctim}
	switch n.kind {
	case KindFile:
		st.Size = int64(len(n.data))
	case KindSymlink:
		st.Size = int64(len(n.target))
	}
	return st
}

// SetTimes sets n's access and modification times. A nil time stays
// unchanged. Ctim becomes the current time if either is set.
func (fs *FS) SetTimes(n *Node, atim, mtim *int64) {
	if atim == nil && mtim == nil {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if atim != nil {
		n.atim = *atim
	}
	if mtim != nil {
		n.mtim = *mtim
	}
	n.ctim = fs.now()
}

// ReadAt reads from n at off. Reading at or past the end returns 0 bytes.
func (fs *FS) ReadAt(n *Node, p []byte, off int64) (int, wasiabi.Errno) {
	switch n.kind {
	case KindDir:
		return 0, wasiabi.EISDIR
	case KindSymlink:
		return 0, wasiabi.EINVAL
	case KindDevice:
		return readDevice(n.dev, p), wasiabi.ESUCCESS
	}
	if off < 0 {
		return 0, wasiabi.EINVAL
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if off >= int64(len(n.data)) {
		return 0, wasiabi.ESUCCESS
	}
	return copy(p, n.data[off:]), wasiabi.ESUCCESS
}

// IsURandom reports whether n is /dev/urandom's device node.
func (fs *FS) IsURandom(n *Node) bool { return n.kind == KindDevice && n.dev == devURandom }

func readDevice(d device, p []byte) int {
	switch d {
	case devZero:
		clear(p)
		return len(p)
	case devURandom:
		rand.Read(p) // never fails, and crashes the program instead
		return len(p)
	}
	return 0 // devNull
}

// WriteAt writes p to n at off, zero-filling any gap past the end. A file
// cannot grow past MaxFileSize, and trying fails with EFBIG. Devices accept
// and discard every write.
func (fs *FS) WriteAt(n *Node, p []byte, off int64) (int, wasiabi.Errno) {
	switch n.kind {
	case KindDir:
		return 0, wasiabi.EISDIR
	case KindSymlink:
		return 0, wasiabi.EINVAL
	case KindDevice:
		return len(p), wasiabi.ESUCCESS
	}
	switch {
	case off < 0:
		return 0, wasiabi.EINVAL
	case len(p) == 0:
		return 0, wasiabi.ESUCCESS // POSIX says this has no effect on a regular file
	case off > MaxFileSize-int64(len(p)):
		return 0, wasiabi.EFBIG
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if end := int(off) + len(p); end > len(n.data) {
		n.data = grow(n.data, end)
	}
	copy(n.data[off:], p)
	n.mtim = fs.now()
	n.ctim = n.mtim
	return len(p), wasiabi.ESUCCESS
}

// Truncate sets n's size, zero-filling when it grows. A size over
// MaxFileSize fails with EFBIG.
func (fs *FS) Truncate(n *Node, size int64) wasiabi.Errno {
	switch {
	case n.kind == KindDir:
		return wasiabi.EISDIR
	case n.kind != KindFile, size < 0:
		return wasiabi.EINVAL
	case size > MaxFileSize:
		return wasiabi.EFBIG
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.truncate(n, int(size))
	return wasiabi.ESUCCESS
}

func (fs *FS) truncate(n *Node, size int) {
	switch {
	case size == 0:
		n.data = nil // release the memory
	case size < len(n.data):
		n.data = n.data[:size]
	default:
		n.data = grow(n.data, size)
	}
	n.mtim = fs.now()
	n.ctim = n.mtim
}

// grow extends b to size bytes, and the new bytes are zero. Capacity past
// len may still hold data from before a truncate, so grow clears it.
func grow(b []byte, size int) []byte {
	old := len(b)
	b = slices.Grow(b, size-old)[:size]
	clear(b[old:])
	return b
}

// Readdir lists dir's entries sorted bytewise by name.
func (fs *FS) Readdir(dir *Node) ([]Dirent, wasiabi.Errno) {
	if dir.kind != KindDir {
		return nil, wasiabi.ENOTDIR
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	ents := make([]Dirent, 0, len(dir.children))
	for name, n := range dir.children {
		ents = append(ents, Dirent{Name: name, Ino: n.ino, Kind: n.kind})
	}
	slices.SortFunc(ents, func(a, b Dirent) int { return strings.Compare(a.Name, b.Name) })
	return ents, wasiabi.ESUCCESS
}

// WriteFile creates or replaces the regular file at path, creating missing
// parent directories. The error is a wasiabi.Errno.
func (fs *FS) WriteFile(path string, data []byte) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return errOf(fs.writeFile(path, data))
}

func (fs *FS) writeFile(path string, data []byte) wasiabi.Errno {
	switch {
	case int64(len(data)) > MaxFileSize:
		return wasiabi.EFBIG
	case len(path) > MaxPathLen:
		return wasiabi.ENAMETOOLONG // before mkdirAll makes any parent
	}
	if err := fs.mkdirAll(parentPath(path)); err != wasiabi.ESUCCESS {
		return err
	}
	n, err := fs.open(fs.root, path, OpenFlags{Create: true, Trunc: true})
	if err != wasiabi.ESUCCESS {
		return err
	}
	if n.kind != KindFile {
		return wasiabi.EINVAL
	}
	n.data = slices.Clone(data)
	n.mtim = fs.now()
	n.ctim = n.mtim
	return wasiabi.ESUCCESS
}

// ReadFile returns a copy of the regular file at path. The error is a
// wasiabi.Errno.
func (fs *FS) ReadFile(path string) ([]byte, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	w := walker{fs: fs}
	n, err := w.lookup(fs.root, path, true)
	switch {
	case err != wasiabi.ESUCCESS:
		return nil, err
	case n.kind == KindDir:
		return nil, wasiabi.EISDIR
	case n.kind != KindFile:
		return nil, wasiabi.EINVAL
	}
	return slices.Clone(n.data), nil
}

// mkdirAll creates the directory path and any missing parents.
func (fs *FS) mkdirAll(path string) wasiabi.Errno {
	for i := 1; i <= len(path); i++ {
		if i < len(path) && path[i] != '/' {
			continue
		}
		if err := fs.mkdir(fs.root, path[:i]); err != wasiabi.ESUCCESS && err != wasiabi.EEXIST {
			return err
		}
	}
	w := walker{fs: fs}
	n, err := w.lookup(fs.root, path, true)
	if err == wasiabi.ESUCCESS && n.kind != KindDir {
		err = wasiabi.ENOTDIR
	}
	return err
}

// parentPath returns path without its last component.
func parentPath(path string) string {
	path = strings.TrimRight(path, "/")
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return "/"
	}
	return path[:i]
}

// newNode returns an unlinked node stamped with the current time.
func (fs *FS) newNode(kind Kind) *Node {
	fs.lastIno++
	now := fs.now()
	n := &Node{ino: fs.lastIno, kind: kind, atim: now, mtim: now, ctim: now}
	if kind == KindDir {
		n.children = map[string]*Node{}
	}
	return n
}

// addEntry names n in dir.
func (fs *FS) addEntry(dir *Node, name string, n *Node) {
	now := fs.now()
	dir.children[name] = n
	n.nlink++
	n.ctim = now
	if n.kind == KindDir {
		n.parent = dir
	}
	dir.mtim, dir.ctim = now, now
}

// removeEntry removes the name of n in dir. n keeps its data and, if it is a
// directory, its parent pointer.
func (fs *FS) removeEntry(dir *Node, name string, n *Node) {
	now := fs.now()
	delete(dir.children, name)
	n.nlink--
	n.ctim = now
	dir.mtim, dir.ctim = now, now
}

// now returns the current time, forced to move strictly forward. Callers
// hold the write lock.
func (fs *FS) now() int64 {
	t := time.Now().UnixNano()
	if t <= fs.clock {
		t = fs.clock + 1
	}
	fs.clock = t
	return t
}

// errOf turns an errno into an error that is nil on success.
func errOf(err wasiabi.Errno) error {
	if err == wasiabi.ESUCCESS {
		return nil
	}
	return err
}
