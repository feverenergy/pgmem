// Package wasi is pgmem's own wasi_snapshot_preview1 host module. Every
// host call finds the calling process's state on its context, so one module
// instance serves every process, and a kernel can create and inherit fds by
// number across processes.
package wasi

import (
	"context"
	"io"

	"github.com/tetratelabs/wazero/api"

	"github.com/feverenergy/pgmem/internal/memfs"
)

// Proc is one process's WASI state. Host functions find it on the call's
// context, where WithProc puts it, so every instance can run the same host
// module.
type Proc struct {
	FS   *memfs.FS
	FDs  *FDTable
	Args []string // Args[0] is the program path
	Env  []string // "KEY=value"
	// Signals are the process's pending signals, which interrupt any
	// blocking call (Wait). If Signals is nil, the process cannot be
	// interrupted.
	Signals Signals
	// AfterCall, if not nil, runs after every host call returns. The
	// kernel uses it for signal dispatch. It runs on the calling goroutine
	// with the call's context and module, after the call's results are
	// stored, so it may call back into the guest.
	AfterCall func(ctx context.Context, mod api.Module)
	// Realtime returns the realtime clock in ns since the Unix epoch. If it
	// is nil, the host's clock is used. The monotonic clock is always the
	// host's.
	Realtime func() int64
	// Random is the source of random_get and of reads from /dev/urandom
	// opened by this process. If it is nil, crypto/rand is used. A read
	// error fails the call with EIO.
	Random io.Reader
}

type procKey struct{}

// WithProc returns ctx carrying p for the host functions.
func WithProc(ctx context.Context, p *Proc) context.Context {
	return context.WithValue(ctx, procKey{}, p)
}

// ProcFrom returns the Proc on ctx, or nil if there is none.
func ProcFrom(ctx context.Context) *Proc {
	p, _ := ctx.Value(procKey{}).(*Proc)
	return p
}

// rootPreopen is the name of the one preopened directory, at preopenFD.
const rootPreopen = "/"

// preopenFD is the fd of the root preopen in every process.
const preopenFD = 3

// NewRootFDs returns a table with fd 0..2 bound to the given streams and
// fd 3 the preopen "/". A nil stdin reads EOF, and a nil stdout or stderr
// discards.
func NewRootFDs(fs *memfs.FS, stdin io.Reader, stdout, stderr io.Writer) *FDTable {
	t := NewFDTable()
	t.InstallAt(0, NewDesc(&readStream{r: stdin}, 0))
	t.InstallAt(1, NewDesc(&writeStream{w: stdout}, 0))
	t.InstallAt(2, NewDesc(&writeStream{w: stderr}, 0))
	t.InstallAt(preopenFD, newPreopen(fs))
	return t
}

// NewChildFDs returns a table with only fd 3, the preopen "/".
func NewChildFDs(fs *memfs.FS) *FDTable {
	t := NewFDTable()
	t.InstallAt(preopenFD, newPreopen(fs))
	return t
}

func newPreopen(fs *memfs.FS) *Desc {
	d := NewDesc(NewNodeFile(fs, fs.Root()), 0)
	d.preopen = rootPreopen
	return d
}
