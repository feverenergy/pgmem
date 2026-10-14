// Package wasmrt configures wazero for pgmem's WebAssembly modules.
package wasmrt

import (
	"context"
	"os"
	"path/filepath"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Features are the WebAssembly features pgmem's modules use. They are
// exception handling for setjmp/longjmp, and threads for atomics.
const Features = api.CoreFeaturesV2 |
	experimental.CoreFeaturesExceptionHandling |
	experimental.CoreFeaturesThreads

// Options configure NewRuntime.
type Options struct {
	// CacheDir holds compiled modules between runs. Empty disables caching.
	CacheDir string

	// CloseOnContextDone makes wazero close a module when the context its
	// call runs under is done, checking on loop back-edges and calls. Without
	// it a guest that makes no host calls cannot be stopped.
	CloseOnContextDone bool

	// Interpreter runs guests on wazero's interpreter instead of compiling
	// them to machine code. It is many times slower, but what a guest
	// computes does not depend on the host's machine code. See
	// kernel.Config.Deterministic. The interpreter does not use CacheDir,
	// so every runtime converts each module again.
	Interpreter bool
}

// DefaultCacheDir is where pgmem keeps compiled modules, in
// <user cache dir>/pgmem/compiled/v2. The version changes whenever the
// runtime configuration changes what wazero stores in an entry. v2 has no
// debug info, whose source maps made every cached load about 10 times
// slower.
func DefaultCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pgmem", "compiled", "v2"), nil
}

// NewRuntime returns a wazero runtime that can compile pgmem's modules. It
// instantiates no host modules, so callers add pgmem's WASI and pgm
// modules. Create instances with a vmem allocator on the context. Close the
// runtime when done.
func NewRuntime(ctx context.Context, opts Options) (wazero.Runtime, error) {
	// Debug info is off. With it, every process exit, which is a panic
	// carrying sys.ExitError, builds a stack trace. Its DWARF line lookups
	// share one mutex per module, so exits queue behind each other under
	// connection churn. Ended backends then stayed alive for up to 1.5 s.
	// Traps keep function names.
	cfg := wazero.NewRuntimeConfig()
	if opts.Interpreter {
		cfg = wazero.NewRuntimeConfigInterpreter()
	}
	cfg = cfg.WithCoreFeatures(Features).WithDebugInfoEnabled(false)
	if opts.CloseOnContextDone {
		cfg = cfg.WithCloseOnContextDone(true)
	}
	if opts.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(opts.CacheDir)
		if err != nil {
			return nil, err
		}
		cfg = cfg.WithCompilationCache(cache)
	}
	return wazero.NewRuntimeWithConfig(ctx, cfg), nil
}
