package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// libDir holds the placeholder files of the statically linked libraries.
const libDir = "/pgmem/lib/postgresql"

// copyIn copies the regular files and directories of the host tree dir
// into fsys at the same absolute paths, and returns the number of files.
// Symlinks and special files are skipped.
func copyIn(fsys *memfs.FS, dir string) (int, error) {
	count := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return mkdirAll(fsys, p)
		case !d.Type().IsRegular():
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := fsys.WriteFile(p, data); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		count++
		return nil
	})
	return count, err
}

// mkdirAll creates the absolute directory path in fsys, with any missing
// parents.
func mkdirAll(fsys *memfs.FS, path string) error {
	cur := ""
	for part := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		cur += "/" + part
		if e := fsys.Mkdir(fsys.Root(), cur); e != wasiabi.ESUCCESS && e != wasiabi.EEXIST {
			return fmt.Errorf("mkdir %s: %w", cur, e)
		}
	}
	return nil
}

// addLibAliases adds <lib>suffix next to each <lib>.so placeholder.
func addLibAliases(fsys *memfs.FS, suffix string) error {
	dir, e := fsys.Lookup(fsys.Root(), libDir, true)
	if e != wasiabi.ESUCCESS {
		return fmt.Errorf("%s: %w", libDir, e)
	}
	ents, e := fsys.Readdir(dir)
	if e != wasiabi.ESUCCESS {
		return fmt.Errorf("%s: %w", libDir, e)
	}
	for _, ent := range ents {
		base, ok := strings.CutSuffix(ent.Name, ".so")
		if !ok || ent.Kind != memfs.KindFile {
			continue
		}
		data, err := fsys.ReadFile(libDir + "/" + ent.Name)
		if err != nil {
			return err
		}
		if err := fsys.WriteFile(libDir+"/"+base+suffix, data); err != nil {
			return err
		}
	}
	return nil
}

// fileSig tells whether a file changed since it was last copied.
type fileSig struct {
	mtime int64
	size  int64
}

// outSyncer keeps directories in sync between memfs and the host. It
// copies the regular files whose modification time or size changed since
// it last copied them. It never deletes. On the host it never follows a
// symlink inside a directory. It skips and logs a symlink, or any other
// file that is not a regular file or directory. It does not reconcile
// concurrent writes to one file from both sides, so the last copy wins.
type outSyncer struct {
	fs   *memfs.FS
	dirs []string
	log  *log.Logger

	mu   sync.Mutex
	mem  map[string]fileSig // memfs file as last copied out or in
	host map[string]fileSig // host file as last written or read
}

func newOutSyncer(fsys *memfs.FS, dirs []string, logger *log.Logger) *outSyncer {
	return &outSyncer{fs: fsys, dirs: dirs, log: logger, mem: map[string]fileSig{}, host: map[string]fileSig{}}
}

// mentions reports whether a query's text names one of the directories.
func (o *outSyncer) mentions(q []byte) bool {
	for _, d := range o.dirs {
		if bytes.Contains(q, []byte(d)) {
			return true
		}
	}
	return false
}

func (o *outSyncer) memSig(p string) (fileSig, bool) {
	n, e := o.fs.Lookup(o.fs.Root(), p, false)
	if e != wasiabi.ESUCCESS {
		return fileSig{}, false
	}
	st := o.fs.Stat(n)
	return fileSig{st.Mtim, st.Size}, st.Kind == memfs.KindFile
}

// syncIn copies host files that changed into memfs. WalkDir does not
// follow symlinks, and only regular files are copied.
func (o *outSyncer) syncIn() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range o.dirs {
		filepath.WalkDir(d, func(p string, ent fs.DirEntry, err error) error {
			if err != nil {
				o.log.Printf("sync in: %v", err)
				return nil
			}
			if ent.IsDir() {
				return nil
			}
			if !ent.Type().IsRegular() {
				o.log.Printf("sync in: skipping %s: not a regular file", p)
				return nil
			}
			info, err := ent.Info()
			if err != nil {
				o.log.Printf("sync in: %v", err)
				return nil
			}
			sig := fileSig{info.ModTime().UnixNano(), info.Size()}
			if o.host[p] == sig {
				return nil
			}
			data, err := os.ReadFile(p)
			if err == nil {
				err = o.fs.WriteFile(p, data)
			}
			if err != nil {
				o.log.Printf("sync in: %s: %v", p, err)
				return nil
			}
			o.host[p] = sig
			if ms, ok := o.memSig(p); ok {
				o.mem[p] = ms
			}
			return nil
		})
	}
}

// syncOut copies memfs files that changed to the host.
func (o *outSyncer) syncOut() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range o.dirs {
		o.walkOut(d)
	}
}

// walkOut copies dir's changed files out. The caller has checked the host
// directory dir, or dir is the -out directory itself.
func (o *outSyncer) walkOut(dir string) {
	n, e := o.fs.Lookup(o.fs.Root(), dir, true)
	if e != wasiabi.ESUCCESS {
		o.log.Printf("sync out: %s: %v", dir, e)
		return
	}
	ents, e := o.fs.Readdir(n)
	if e != wasiabi.ESUCCESS {
		o.log.Printf("sync out: %s: %v", dir, e)
		return
	}
	for _, ent := range ents {
		p := dir + "/" + ent.Name
		switch ent.Kind {
		case memfs.KindDir:
			if err := hostMkdir(p); err != nil {
				o.log.Printf("sync out: %v", err)
				continue
			}
			o.walkOut(p)
		case memfs.KindFile:
			sig, ok := o.memSig(p) // before reading, so a later write shows as a change
			if !ok || o.mem[p] == sig {
				continue
			}
			data, err := o.fs.ReadFile(p)
			if err == nil {
				err = hostWrite(p, data)
			}
			if err != nil {
				o.log.Printf("sync out: %s: %v", p, err)
				continue
			}
			o.mem[p] = sig
			if info, err := os.Lstat(p); err == nil {
				o.host[p] = fileSig{info.ModTime().UnixNano(), info.Size()}
			}
		}
	}
}

// hostMkdir makes sure p is a real directory on the host, not a symlink.
func hostMkdir(p string) error {
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.Mkdir(p, 0o755)
	case err != nil:
		return err
	case !fi.IsDir(): // includes a symlink to a directory
		return fmt.Errorf("skipping %s: not a directory (%v)", p, fi.Mode().Type())
	}
	return nil
}

// hostWrite writes a regular host file, refusing to follow a symlink or
// to open anything else that is not a regular file (such as a FIFO).
func hostWrite(p string, data []byte) error {
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file (%v)", fi.Mode().Type())
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}
