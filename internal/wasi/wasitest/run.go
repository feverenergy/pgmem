// Package wasitest runs WebAssembly programs on pgmem's WASI in tests,
// without the kernel.
package wasitest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/sys"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// Result is what a program printed and how it exited.
type Result struct {
	Stdout, Stderr string
	ExitCode       uint32
	Trapped        bool
}

// Options describe the process wasitest.Run creates.
type Options struct {
	FS    *memfs.FS           // nil means memfs.New()
	Extra map[int32]wasi.File // installed over the root table before start
}

var instances atomic.Int64

// Run runs wasm on pgmem's WASI without the kernel. It uses a fresh
// runtime and vmem allocator, the root fds plus opts.Extra, and the
// environment PGMEM_FIXTURE=yes, which every program run here sees.
// args[0] is the program path. A trap sets Trapped instead of failing the
// test, so tests can assert on traps.
func Run(t testing.TB, wasm []byte, opts Options, args ...string) Result {
	t.Helper()
	var alloc vmem.Allocator
	ctx := experimental.WithMemoryAllocator(context.Background(), &alloc)
	r := newRuntime(ctx, t)
	defer r.Close(ctx)

	fs := opts.FS
	if fs == nil {
		fs = memfs.New()
	}
	var stdout, stderr bytes.Buffer
	p := &wasi.Proc{
		FS:   fs,
		FDs:  wasi.NewRootFDs(fs, nil, &stdout, &stderr),
		Args: args,
		Env:  []string{"PGMEM_FIXTURE=yes"},
	}
	defer p.FDs.CloseAll()
	for fd, f := range opts.Extra {
		p.FDs.InstallAt(fd, wasi.NewDesc(f, 0))
	}

	name := fmt.Sprintf("wasitest-%d", instances.Add(1))
	_, err := r.InstantiateWithConfig(wasi.WithProc(ctx, p), wasm, wazero.NewModuleConfig().WithName(name))
	var res Result
	var exitErr *sys.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		res.Trapped = true
		t.Logf("%s trapped: %v", args[0], err)
	}
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	return res
}

// newRuntime returns a runtime with pgmem's WASI instantiated.
func newRuntime(ctx context.Context, t testing.TB) wazero.Runtime {
	t.Helper()
	cacheDir, err := wasmrt.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	r, err := wasmrt.NewRuntime(ctx, wasmrt.Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := wasi.Instantiate(ctx, r); err != nil {
		r.Close(ctx)
		t.Fatal(err)
	}
	return r
}
