package memfs

import (
	"strings"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

const (
	maxNameLen  = 255 // longest path component
	maxSymlinks = 40  // symlink hops allowed in one resolution
)

// MaxPathLen is the longest path any call accepts (PATH_MAX). A longer one
// fails with ENAMETOOLONG before it is split.
const MaxPathLen = 4096

// MaxSymlinkLen is the longest symlink target, PATH_MAX less the NUL.
const MaxSymlinkLen = MaxPathLen - 1

// target is a resolved path minus its last component.
type target struct {
	dir     *Node  // directory holding the last component
	name    string // last component, which is a child name, "." or ".."
	mustDir bool   // trailing slash, or the path ends in "." or ".."
}

// walker resolves paths. It counts symlink hops across one whole resolution,
// including the recursive resolutions of link targets. Callers hold fs.mu.
type walker struct {
	fs   *FS
	hops int
}

// splitPath drops empty components and every "." except a final one. The
// path "/" yields no components. A path over MaxPathLen fails with
// ENAMETOOLONG.
func splitPath(p string) ([]string, wasiabi.Errno) {
	if len(p) > MaxPathLen {
		return nil, wasiabi.ENAMETOOLONG
	}
	raw := strings.Split(p, "/")
	comps := make([]string, 0, len(raw))
	for i, c := range raw {
		if len(c) > maxNameLen {
			return nil, wasiabi.ENAMETOOLONG
		}
		if c == "" || (c == "." && i != len(raw)-1) {
			continue
		}
		comps = append(comps, c)
	}
	return comps, wasiabi.ESUCCESS
}

// parent resolves every component of p but the last, following symlinks.
// A path with no components, such as "/", names the start directory as
// ".".
func (w *walker) parent(dir *Node, p string) (target, wasiabi.Errno) {
	if p == "" {
		return target{}, wasiabi.ENOENT
	}
	cur := dir
	if p[0] == '/' {
		cur = w.fs.root
	}
	comps, err := splitPath(p)
	if err != wasiabi.ESUCCESS {
		return target{}, err
	}
	last := "."
	if n := len(comps); n > 0 {
		last, comps = comps[n-1], comps[:n-1]
	}
	for _, c := range comps {
		if cur.kind != KindDir {
			return target{}, wasiabi.ENOTDIR
		}
		if cur, err = w.step(cur, c); err != wasiabi.ESUCCESS {
			return target{}, err
		}
	}
	if cur.kind != KindDir {
		return target{}, wasiabi.ENOTDIR
	}
	mustDir := strings.HasSuffix(p, "/") || isDot(last)
	return target{dir: cur, name: last, mustDir: mustDir}, wasiabi.ESUCCESS
}

// step looks up one component in dir and follows it if it is a symlink.
func (w *walker) step(dir *Node, name string) (*Node, wasiabi.Errno) {
	n := child(dir, name)
	if n == nil {
		return nil, wasiabi.ENOENT
	}
	if n.kind == KindSymlink {
		return w.follow(dir, n)
	}
	return n, wasiabi.ESUCCESS
}

// follow resolves the symlink link, found in dir. A relative target resolves
// from dir.
func (w *walker) follow(dir, link *Node) (*Node, wasiabi.Errno) {
	w.hops++
	if w.hops > maxSymlinks {
		return nil, wasiabi.ELOOP
	}
	return w.lookup(dir, link.target, true)
}

// lookup resolves p. It follows a symlink in the last component when
// follow is set or the path must name a directory.
func (w *walker) lookup(dir *Node, p string, follow bool) (*Node, wasiabi.Errno) {
	t, err := w.parent(dir, p)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	n := child(t.dir, t.name)
	if n == nil {
		return nil, wasiabi.ENOENT
	}
	if n.kind == KindSymlink && (follow || t.mustDir) {
		if n, err = w.follow(t.dir, n); err != wasiabi.ESUCCESS {
			return nil, err
		}
	}
	if t.mustDir && n.kind != KindDir {
		return nil, wasiabi.ENOTDIR
	}
	return n, wasiabi.ESUCCESS
}

// child returns dir's entry name, or nil. "." is dir and ".." its parent.
func child(dir *Node, name string) *Node {
	switch name {
	case ".":
		return dir
	case "..":
		return dir.parent
	}
	return dir.children[name]
}

func isDot(name string) bool { return name == "." || name == ".." }
