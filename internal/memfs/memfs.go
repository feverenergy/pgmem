// Package memfs is pgmem's in-memory file tree, shared by every process.
// Errors are WASI errno values. All methods are safe for concurrent use.
package memfs

import (
	"crypto/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// ino, kind and dev never change.
	ino  uint64
	kind Kind
	dev  device

	// dmu guards data and the times. A caller that holds FS.mu takes dmu
	// inside it, never the other way round, and never holds two nodes'
	// dmu at once.
	dmu              sync.RWMutex
	data             fileData // KindFile
	atim, mtim, ctim int64

	// FS.mu guards the rest.
	nlink    uint64
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

// FS is an in-memory file tree. mu guards the tree itself: names, links,
// parents and symlink targets. Read-only tree operations take its read
// lock, and the rest take its write lock. File data and times have their
// own lock per node (Node.dmu), so processes copying different files, as
// CREATE DATABASE does under parallel tests, do not take turns on mu for
// every read and write.
//
// gate keeps WriteTar's copy at one moment in time. WriteAt, Truncate and
// SetTimes, which change a node without mu's write lock, hold gate's read
// lock. WriteTar holds gate's write lock and mu's read lock, so every
// change waits until the copy is done. The lock order is gate, mu, dmu.
type FS struct {
	gate    sync.RWMutex
	mu      sync.RWMutex
	root    *Node
	lastIno uint64       // inode numbers are never reused
	clock   atomic.Int64 // last time handed out by now
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
	n.dmu.RLock()
	defer n.dmu.RUnlock()
	st := Stat{Ino: n.ino, Kind: n.kind, Nlink: n.nlink, Atim: n.atim, Mtim: n.mtim, Ctim: n.ctim}
	switch n.kind {
	case KindFile:
		st.Size = int64(n.data.size)
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
	fs.gate.RLock()
	defer fs.gate.RUnlock()
	n.dmu.Lock()
	defer n.dmu.Unlock()
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
	n.dmu.RLock()
	defer n.dmu.RUnlock()
	if off >= int64(n.data.size) {
		return 0, wasiabi.ESUCCESS
	}
	return n.data.readAt(p, int(off)), wasiabi.ESUCCESS
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
	fs.gate.RLock()
	defer fs.gate.RUnlock()
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if end := int(off) + len(p); end > n.data.size {
		n.data.resize(end)
	}
	n.data.writeAt(p, int(off))
	n.stamp(fs.now())
	return len(p), wasiabi.ESUCCESS
}

// stamp sets the modification and status-change times to t. The caller
// holds n.dmu.
func (n *Node) stamp(t int64) {
	n.mtim, n.ctim = t, t
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
	fs.gate.RLock()
	defer fs.gate.RUnlock()
	fs.truncate(n, int(size))
	return wasiabi.ESUCCESS
}

// truncate sets n's size and stamps it in one hold of n.dmu. Truncate
// calls it under gate, and open calls it under mu's write lock.
func (fs *FS) truncate(n *Node, size int) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	n.data.resize(size)
	n.stamp(fs.now())
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
	if int64(len(data)) > MaxFileSize {
		return wasiabi.EFBIG
	}
	var d fileData
	d.resize(len(data))
	d.writeAt(data, 0)
	return fs.putFile(path, d)
}

// putFile creates or replaces the regular file at path, creating missing
// parent directories, and gives it the contents d. The file takes d's
// pages, so the caller must not use d again.
func (fs *FS) putFile(path string, d fileData) wasiabi.Errno {
	switch {
	case int64(d.size) > MaxFileSize:
		return wasiabi.EFBIG
	case len(path) > MaxPathLen:
		return wasiabi.ENAMETOOLONG // before mkdirAll makes any parent
	}
	if err := fs.mkdirAll(parentPath(path)); err != wasiabi.ESUCCESS {
		return err
	}
	// Open without Trunc and swap the data in one hold of n.dmu, so a
	// reader of an existing file sees the old contents or the new, never
	// an empty file in between.
	n, err := fs.open(fs.root, path, OpenFlags{Create: true})
	switch {
	case err != wasiabi.ESUCCESS:
		return err
	case n.kind == KindDir:
		return wasiabi.EISDIR
	case n.kind != KindFile:
		return wasiabi.EINVAL
	}
	n.dmu.Lock()
	defer n.dmu.Unlock()
	n.data = d
	n.stamp(fs.now())
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
	n.dmu.RLock()
	defer n.dmu.RUnlock()
	return n.data.bytes(), nil
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

// addEntry names n in dir. The caller holds mu's write lock.
func (fs *FS) addEntry(dir *Node, name string, n *Node) {
	dir.children[name] = n
	n.nlink++
	if n.kind == KindDir {
		n.parent = dir
	}
	fs.setCtim(n)
	fs.lockAndStamp(dir)
}

// removeEntry removes the name of n in dir. n keeps its data and, if it is a
// directory, its parent pointer.
func (fs *FS) removeEntry(dir *Node, name string, n *Node) {
	delete(dir.children, name)
	n.nlink--
	fs.setCtim(n)
	fs.lockAndStamp(dir)
}

// setCtim sets n's status-change time to now under n.dmu. It reads the
// clock inside the lock, so a WriteAt on n can't stamp a later time in
// between and leave ctim behind mtim.
func (fs *FS) setCtim(n *Node) {
	n.dmu.Lock()
	n.ctim = fs.now()
	n.dmu.Unlock()
}

// lockAndStamp takes n.dmu and stamps n with the time now, read inside the
// lock for the same reason as setCtim.
func (fs *FS) lockAndStamp(n *Node) {
	n.dmu.Lock()
	n.stamp(fs.now())
	n.dmu.Unlock()
}

// now returns the current time, forced to move strictly forward across
// every caller, whichever lock it holds.
func (fs *FS) now() int64 {
	for {
		last := fs.clock.Load()
		t := time.Now().UnixNano()
		if t <= last {
			t = last + 1
		}
		if fs.clock.CompareAndSwap(last, t) {
			return t
		}
	}
}

// errOf turns an errno into an error that is nil on success.
func errOf(err wasiabi.Errno) error {
	if err == wasiabi.ESUCCESS {
		return nil
	}
	return err
}
