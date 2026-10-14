package pgmem

import (
	"context"
	"fmt"
	"time"

	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// Kernel exposes the server's kernel to tests, so they can signal guest
// pids and count processes.
func Kernel(s *Server) *kernel.Kernel { return s.k }

// PostmasterPID returns the postmaster's guest pid.
func PostmasterPID(s *Server) int32 { return s.pm.PID() }

// StartPreempt is Start with kernel.Config.Preempt on, for benchmarks that
// compare the two.
func StartPreempt(ctx context.Context, opts Options) (*Server, error) {
	return start(ctx, opts, true)
}

// DirSize sums the sizes of the regular files under path, recursively, in
// s's in-memory file system.
func DirSize(s *Server, path string) (int64, error) {
	n, errno := s.fs.Lookup(s.fs.Root(), path, true)
	if errno != wasiabi.ESUCCESS {
		return 0, fmt.Errorf("lookup %s: %w", path, errno)
	}
	return dirSize(s.fs, n, path)
}

func dirSize(fsys *memfs.FS, dir *memfs.Node, path string) (int64, error) {
	ents, errno := fsys.Readdir(dir)
	if errno != wasiabi.ESUCCESS {
		return 0, fmt.Errorf("readdir %s: %w", path, errno)
	}
	var total int64
	for _, e := range ents {
		n, errno := fsys.Lookup(dir, e.Name, false)
		if errno == wasiabi.ENOENT {
			continue // removed since Readdir
		}
		if errno != wasiabi.ESUCCESS {
			return 0, fmt.Errorf("lookup %s/%s: %w", path, e.Name, errno)
		}
		switch e.Kind {
		case memfs.KindFile:
			total += fsys.Stat(n).Size
		case memfs.KindDir:
			sub, err := dirSize(fsys, n, path+"/"+e.Name)
			if err != nil {
				return 0, err
			}
			total += sub
		}
	}
	return total, nil
}

// BuildDirLoads returns how many times a build has been loaded from
// PGMEM_BUILD_DIR rather than from an engine. Start loads one. The
// build ID (buildid.ID) loads one on its first successful call for a
// directory, and on every failed one.
func BuildDirLoads() int64 { return buildDirLoads.Load() }

// SetCacheDir makes Start use dir as its compile cache directory until
// restore is called.
func SetCacheDir(dir string) (restore func()) {
	old := cacheDir
	cacheDir = func() (string, error) { return dir, nil }
	return func() { cacheDir = old }
}

// SetCloseWaits sets Close's bounds for fast shutdown, immediate shutdown
// and the kernel's close, and a function that receives the result of a
// kernel close left running in the background, until restore is called.
func SetCloseWaits(fast, quit, kernel time.Duration, closed func(error)) (restore func()) {
	f, q, k, c := fastShutdownWait, quitWait, kernelCloseWait, backgroundClosed
	fastShutdownWait, quitWait, kernelCloseWait, backgroundClosed = fast, quit, kernel, closed
	return func() { fastShutdownWait, quitWait, kernelCloseWait, backgroundClosed = f, q, k, c }
}

// EngineFiles returns e's decompressed postgres.wasm, share.tar and
// cluster.tar, as Start loads them when PGMEM_BUILD_DIR is unset.
func EngineFiles(e Engine) (postgres, share, cluster []byte, err error) {
	f, err := e.files()
	if err != nil {
		return nil, nil, nil, err
	}
	if postgres, err = f.postgres(); err != nil {
		return nil, nil, nil, err
	}
	if share, err = f.share(); err != nil {
		return nil, nil, nil, err
	}
	if cluster, err = f.cluster(); err != nil {
		return nil, nil, nil, err
	}
	return postgres, share, cluster, nil
}
