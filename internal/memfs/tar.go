package memfs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// LoadTar unpacks the tar stream r under the directory at, creating missing
// parents. It handles directories, regular files, hard links and symlinks.
// A hard link points to an earlier entry, with both names relative to at.
// It stores symlinks as-is. Entry names cannot climb out of at. Any other
// entry type is an error.
func (fs *FS) LoadTar(r io.Reader, at string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("memfs: read tar: %w", err)
		}
		var data []byte
		if h.Typeflag == tar.TypeReg {
			if data, err = io.ReadAll(tr); err != nil {
				return fmt.Errorf("memfs: read tar entry %s: %w", h.Name, err)
			}
		}
		if err := fs.loadEntry(h, data, at); err != nil {
			return fmt.Errorf("memfs: tar entry %s: %w", h.Name, err)
		}
	}
}

func (fs *FS) loadEntry(h *tar.Header, data []byte, at string) error {
	name := under(at, h.Name)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var err wasiabi.Errno
	switch h.Typeflag {
	case tar.TypeDir:
		err = fs.mkdirAll(name)
	case tar.TypeReg:
		err = fs.writeFile(name, data)
	case tar.TypeLink:
		if err = fs.mkdirAll(parentPath(name)); err == wasiabi.ESUCCESS {
			err = fs.link(fs.root, under(at, h.Linkname), fs.root, name, false)
		}
	case tar.TypeSymlink:
		if err = fs.mkdirAll(parentPath(name)); err == wasiabi.ESUCCESS {
			err = fs.symlink(h.Linkname, fs.root, name)
		}
	default:
		return fmt.Errorf("unsupported entry type %q", h.Typeflag)
	}
	return errOf(err)
}

// under joins the archive name p to at without letting ".." leave at.
func under(at, p string) string {
	return path.Join(at, path.Clean("/"+p))
}

// WriteTar writes the tree under the absolute path dir to w as a tar
// archive, with entry names relative to dir. The output depends only on the
// tree's contents:
//
//   - entries are sorted by name;
//   - directories, empty ones included, end in "/";
//   - symlinks are TypeSymlink;
//   - a node seen again under another name is a TypeLink to its first name;
//   - modes are 0700 for directories and 0600 for files;
//   - owners are 0 and times are zero.
func (fs *FS) WriteTar(w io.Writer, dir string) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	wk := walker{fs: fs}
	n, err := wk.lookup(fs.root, dir, true)
	if err != wasiabi.ESUCCESS {
		return fmt.Errorf("memfs: write tar %s: %w", dir, err)
	}
	if n.kind != KindDir {
		return fmt.Errorf("memfs: write tar %s: %w", dir, wasiabi.ENOTDIR)
	}
	tw := tar.NewWriter(w)
	if err := writeTarDir(tw, n, "", map[*Node]string{}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("memfs: write tar: %w", err)
	}
	return nil
}

// writeTarDir emits the children of d, depth first in name order. prefix is
// the archive name of d with a trailing slash, or "" for the top directory.
// seen maps each regular file to the name of its first entry.
func writeTarDir(tw *tar.Writer, d *Node, prefix string, seen map[*Node]string) error {
	names := make([]string, 0, len(d.children))
	for name := range d.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := d.children[name]
		full := prefix + name
		h := &tar.Header{Name: full, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		switch c.kind {
		case KindDir:
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, full+"/", 0o700
		case KindSymlink:
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, c.target, 0o600
		case KindFile:
			if first, ok := seen[c]; ok {
				h.Typeflag, h.Linkname, h.Mode = tar.TypeLink, first, 0o600
				break
			}
			seen[c] = full
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o600, int64(len(c.data))
		default:
			return fmt.Errorf("memfs: write tar: %s: unsupported node kind %d", full, c.kind)
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("memfs: write tar %s: %w", full, err)
		}
		switch c.kind {
		case KindDir:
			if err := writeTarDir(tw, c, full+"/", seen); err != nil {
				return err
			}
		case KindFile:
			if h.Typeflag == tar.TypeReg {
				if _, err := tw.Write(c.data); err != nil {
					return fmt.Errorf("memfs: write tar %s: %w", full, err)
				}
			}
		}
	}
	return nil
}
