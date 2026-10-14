package wasmrt_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feverenergy/pgmem/internal/wasmrt"
	"github.com/feverenergy/pgmem/internal/wasmrt/wasmrttest"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	wasm, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("%v (run make fixtures)", err)
	}
	return wasm
}

func TestSetjmpLongjmpUnwindsFrames(t *testing.T) {
	res := wasmrttest.Run(t, readFixture(t, "sjlj.wasm"), wasmrttest.Options{}, "/sjlj")
	if !res.Status.Exited() || res.Status.ExitCode() != 0 || res.Stdout != "longjmp 42\n" {
		t.Fatalf("status %#x, stdout %q, stderr %q", res.Status, res.Stdout, res.Stderr)
	}
}

func TestThreadLocalErrno(t *testing.T) {
	res := wasmrttest.Run(t, readFixture(t, "errno.wasm"), wasmrttest.Options{}, "/errno")
	if !res.Status.Exited() || res.Status.ExitCode() != 0 || res.Stdout != "open=-1 errno=44\n" {
		t.Fatalf("status %#x, stdout %q, stderr %q", res.Status, res.Stdout, res.Stderr)
	}
}

// The cache directory is versioned. Entries compiled with wazero's debug
// info must never be reused now that it is off. Its DWARF source maps are
// slow to load.
func TestDefaultCacheDirVersioned(t *testing.T) {
	dir, err := wasmrt.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "v2" || filepath.Base(filepath.Dir(dir)) != "compiled" {
		t.Fatalf("cache dir %q, want .../pgmem/compiled/v2", dir)
	}
}
