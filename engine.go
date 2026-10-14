package pgmem

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"

	"github.com/feverenergy/pgmem-engine/pg17"
	"github.com/klauspost/compress/zstd"
)

// Engine is a PostgreSQL build that Start runs. FS must hold three
// zstd-compressed files at its root:
//
//   - postgres.wasm.zst, the server compiled to WebAssembly;
//   - share.tar.zst, a tar of Postgres's share/ and lib/ trees;
//   - cluster.tar.zst, a tar of a fresh cluster's data directory.
//
// FS may also hold SHA256SUMS and INPUTS.SHA256SUMS, which list the
// sha256 of the compressed and the decompressed files in sha256sum
// format. Start does not read them.
//
// Version names the engine. pgmem decompresses each file of an engine on
// its first use and keeps it, and the build ID derived from the files,
// under the Version for the life of the process. A file that cannot be
// read or decompressed is kept as that error, so every later Start with
// the engine fails the same way.
//
// Each distinct engine therefore needs a distinct Version. An Engine
// whose Version is already in use with another FS is an error, when pgmem
// can compare the two FS values. It cannot compare two maps, such as two
// fstest.MapFS, and then the second Engine gets the first one's files.
// So pass the same FS value every time, for example from a package
// variable. fs.Sub returns a new value on each call, and an Engine built
// from a second call is an error.
//
// The zero Engine means Postgres17. An Engine with only one of Version and
// FS set is an error.
type Engine struct {
	Version string
	FS      fs.FS
}

// Postgres17 is PostgreSQL 17, from the module
// github.com/feverenergy/pgmem-engine/pg17. Start uses it when
// Options.Engine is the zero Engine.
var Postgres17 = Engine{Version: pg17.Version, FS: pg17.FS}

// engineFiles holds one engine's decompressed files. Each func
// decompresses its file on the first call and returns the same slice to
// every caller, so callers must not modify it. A failed call's error is
// kept too. fsys is the FS the files come from.
type engineFiles struct {
	fsys                     fs.FS
	postgres, share, cluster func() ([]byte, error)
}

// engines maps each Version used in this process to its *engineFiles.
// The key is the Version and not the Engine, because an fs.FS may be a
// map, such as fstest.MapFS, and a map key that holds one panics.
var engines sync.Map

// resolve returns e, or Postgres17 when e is the zero Engine.
func (e Engine) resolve() Engine {
	if e.Version == "" && e.FS == nil {
		return Postgres17
	}
	return e
}

// files returns e's decompressed files. It decompresses nothing itself.
// Each file is decompressed on its first use.
func (e Engine) files() (*engineFiles, error) {
	e = e.resolve()
	switch {
	case e.Version == "":
		return nil, errors.New("pgmem: engine has no Version")
	case e.FS == nil:
		return nil, fmt.Errorf("pgmem: engine %q has no FS", e.Version)
	}
	f, ok := engines.Load(e.Version)
	if !ok {
		file := func(name string) func() ([]byte, error) {
			return sync.OnceValues(func() ([]byte, error) { return decode(e, name) })
		}
		f, _ = engines.LoadOrStore(e.Version, &engineFiles{
			fsys:     e.FS,
			postgres: file("postgres.wasm.zst"),
			share:    file("share.tar.zst"),
			cluster:  file("cluster.tar.zst"),
		})
	}
	ef := f.(*engineFiles)
	if otherFS(ef.fsys, e.FS) {
		return nil, fmt.Errorf("pgmem: engine %q: another engine with this Version and a different FS is in use", e.Version)
	}
	return ef, nil
}

// otherFS reports whether a and b are different file systems. When the
// comparison panics, because a and b hold the same type and it is not
// comparable, otherFS cannot tell and returns false.
func otherFS(a, b fs.FS) (other bool) {
	defer func() {
		if recover() != nil {
			other = false
		}
	}()
	return a != b
}

// decode reads name from e.FS and decompresses it. Its errors name the
// engine's Version and the file.
func decode(e Engine, name string) ([]byte, error) {
	src, err := fs.ReadFile(e.FS, name)
	if err != nil {
		var pe *fs.PathError
		if !errors.As(err, &pe) { // a PathError names the file already
			err = fmt.Errorf("%s: %w", name, err)
		}
		return nil, fmt.Errorf("pgmem: engine %q: %w", e.Version, err)
	}
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("pgmem: engine %q: %s: %w", e.Version, name, err)
	}
	defer d.Close()
	out, err := d.DecodeAll(src, nil)
	if err != nil {
		return nil, fmt.Errorf("pgmem: engine %q: %s: %w", e.Version, name, err)
	}
	return out, nil
}
