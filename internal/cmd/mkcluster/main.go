// mkcluster builds a pgmem cluster image. It runs initdb inside pgmem on
// the output of make wasm and writes the data directory as a tar. The
// kernel is deterministic, so the image is the same on every run and every
// host. Each process gets a fixed clock and fixed random bytes, timers
// never fire, and guests run on the wazero interpreter.
//
// There is one caveat. The interpreter computes floats with the host's
// instructions, so a NaN's payload bits follow the host CPU. No computed
// NaN reaches today's image. That was verified on darwin/arm64 and
// emulated linux/amd64. See kernel.Config.Deterministic. The interpreter
// ignores the compile cache, so every run takes about 15 s on an M-series
// Mac.
//
//	go run ./internal/cmd/mkcluster -wasm build/out -o build/out/cluster.tar
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/feverenergy/pgmem/internal/cluster"
	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

func main() {
	wasmDir := flag.String("wasm", "build/out", "directory with postgres.wasm, initdb.wasm and share.tar")
	out := flag.String("o", "build/out/cluster.tar", "cluster image to write")
	flag.Parse()
	if err := run(*wasmDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "mkcluster:", err)
		os.Exit(1)
	}
}

func run(wasmDir, out string) error {
	start := time.Now()
	ctx := context.Background()
	b, err := cluster.LoadBuild(wasmDir)
	if err != nil {
		return err
	}
	cache, err := wasmrt.DefaultCacheDir()
	if err != nil {
		return err
	}
	fsys := memfs.New()
	k, err := kernel.New(ctx, kernel.Config{FS: fsys, CacheDir: cache, Deterministic: true})
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = k.Close(cctx)
	}()
	if err := cluster.Install(ctx, k, fsys, b); err != nil {
		return err
	}
	var log bytes.Buffer
	if err := cluster.Create(ctx, k, fsys, &log); err != nil {
		return fmt.Errorf("%w\n%s", err, log.String())
	}
	stdout, stderr, st, err := cluster.Single(ctx, k, "SELECT 1;\n")
	if err != nil {
		return err
	}
	if !st.Exited() || st.ExitCode() != 0 || !strings.Contains(stdout, `?column? = "1"`) {
		return fmt.Errorf("new cluster does not answer SELECT 1 (status %#x)\n%s%s", st, stdout, stderr)
	}
	var img bytes.Buffer
	if err := fsys.WriteTar(&img, cluster.DataDir); err != nil {
		return err
	}
	if err := writeAtomic(out, img.Bytes()); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes) in %v\n", out, img.Len(), time.Since(start).Round(time.Millisecond))
	return nil
}

// writeAtomic writes data to path through a temporary file in the same
// directory, so path is either the old file or the complete new one.
func writeAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
