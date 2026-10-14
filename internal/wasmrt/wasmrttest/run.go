// Package wasmrttest runs WebAssembly programs in tests.
package wasmrttest

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// Options describe the files a program sees. Files maps absolute paths to
// contents. Run creates missing parents.
type Options struct {
	Files map[string][]byte
	Stdin io.Reader
}

// closeTimeout bounds Kernel.Close at the end of Run.
const closeTimeout = 10 * time.Second

// Result is what a program printed and how it exited.
type Result struct {
	Stdout, Stderr string
	Status         kernel.Status
}

// Run runs wasm as a pgmem process on a fresh kernel and memfs. args[0] is
// the program path. Run registers the module there and creates the file
// with the wasm bytes, because Postgres looks for its own executable. Traps
// are a status, not a test failure.
func Run(t testing.TB, wasm []byte, opts Options, args ...string) Result {
	t.Helper()
	ctx := context.Background()
	fsys := memfs.New()
	for path, data := range opts.Files {
		if err := fsys.WriteFile(path, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsys.WriteFile(args[0], wasm); err != nil {
		t.Fatal(err)
	}
	cacheDir, err := wasmrt.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kernel.New(ctx, kernel.Config{FS: fsys, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Every process has been reaped, so Close only closes the runtime.
		// The bound keeps a regression from hanging the test.
		cctx, cancel := context.WithTimeout(ctx, closeTimeout)
		defer cancel()
		if err := k.Close(cctx); err != nil {
			t.Errorf("close kernel: %v", err)
		}
	}()
	m, err := k.Compile(ctx, wasm)
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	if err := k.Register(args[0], m); err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	var stdout, stderr bytes.Buffer
	p, err := k.Start(ctx, kernel.Spec{
		Path: args[0], Args: args[1:],
		Stdin: opts.Stdin, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	st := p.Wait()
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), Status: st}
}
