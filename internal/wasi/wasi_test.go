package wasi_test

import (
	"os"
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasi/wasitest"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	wasm, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("%v (run make fixtures)", err)
	}
	return wasm
}

func TestFileSystemCalls(t *testing.T) {
	res := wasitest.Run(t, fixture(t, "fsops.wasm"), wasitest.Options{}, "/fsops")
	if res.ExitCode != 0 || res.Stdout != "fsops ok\n" {
		t.Fatalf("exit %d trapped %v\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Trapped, res.Stdout, res.Stderr)
	}
}

func TestPrestatOnlyForPreopens(t *testing.T) {
	fs := memfs.New()
	null, _ := fs.Lookup(fs.Root(), "/dev/null", true)
	res := wasitest.Run(t, fixture(t, "prestat.wasm"), wasitest.Options{FS: fs, Extra: map[int32]wasi.File{4: wasi.NewNodeFile(fs, null)}}, "/prestat")
	if res.Stdout != "prestat(4)=8 prestat(3)=0\n" {
		t.Fatalf("%q %q", res.Stdout, res.Stderr)
	}
}

func TestExitCodeAndTrap(t *testing.T) {
	wasm := fixture(t, "exit.wasm")
	if res := wasitest.Run(t, wasm, wasitest.Options{}, "/exit", "3"); res.ExitCode != 3 || res.Trapped {
		t.Fatalf("exit 3: got exit %d trapped %v", res.ExitCode, res.Trapped)
	}
	if res := wasitest.Run(t, wasm, wasitest.Options{}, "/exit", "trap"); !res.Trapped {
		t.Fatalf("trap: got exit %d trapped %v", res.ExitCode, res.Trapped)
	}
}
