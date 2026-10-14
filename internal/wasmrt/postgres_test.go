package wasmrt_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/feverenergy/pgmem/internal/wasmrt"
	"github.com/feverenergy/pgmem/internal/wasmrt/wasmrttest"
)

// readBuild returns a module from make wasm's output in $PGMEM_BUILD_OUT,
// else $PGMEM_BUILD_DIR, else this module's build/out. Without
// PGMEM_REQUIRE_BUILD a missing build skips the test.
func readBuild(t *testing.T, name string) []byte {
	t.Helper()
	dir := os.Getenv("PGMEM_BUILD_OUT")
	if dir == "" {
		dir = os.Getenv("PGMEM_BUILD_DIR")
	}
	if dir == "" {
		dir = filepath.Join("..", "..", "build", "out")
	}
	wasm, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("PGMEM_REQUIRE_BUILD") == "" {
		t.Skipf("%s is not built in %s; run make wasm (PGMEM_BUILD_DIR must be absolute: tests run in their package directory)", name, dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func pinnedPostgresVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "build", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "PG_VERSION="); ok {
			return v
		}
	}
	t.Fatal("PG_VERSION is missing from build/versions.env")
	return ""
}

// TestModulesImportOnlyWASIAndPgm also checks postgres.wasm's initial
// memory, so the module compiles once, uncached, for both checks.
func TestModulesImportOnlyWASIAndPgm(t *testing.T) {
	ctx := context.Background()
	r, err := wasmrt.NewRuntime(ctx, wasmrt.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	for _, name := range []string{"postgres.wasm", "initdb.wasm"} {
		m, err := r.CompileModule(ctx, readBuild(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, f := range m.ImportedFunctions() {
			if mod, fn, _ := f.Import(); mod != "wasi_snapshot_preview1" && mod != "pgm" {
				t.Errorf("%s imports %s.%s; add it to postgres/compat", name, mod, fn)
			}
		}
		if _, ok := m.ExportedFunctions()["pgmem_abi_version"]; !ok {
			t.Errorf("%s does not export pgmem_abi_version", name)
		}
		if name == "postgres.wasm" {
			checkInitialMemoryBelowShmemAddr(t, m)
		}
	}
}

func TestPostgresVersion(t *testing.T) {
	res := wasmrttest.Run(t, readBuild(t, "postgres.wasm"), wasmrttest.Options{}, "/pgmem/bin/postgres", "--version")
	want := "postgres (PostgreSQL) " + pinnedPostgresVersion(t) + "\n"
	if !res.Status.Exited() || res.Status.ExitCode() != 0 || res.Stdout != want {
		t.Fatalf("status %#x, stdout %q, want %q, stderr %q", res.Status, res.Stdout, want, res.Stderr)
	}
}

func TestPostgresDescribeConfig(t *testing.T) {
	res := wasmrttest.Run(t, readBuild(t, "postgres.wasm"), wasmrttest.Options{}, "/pgmem/bin/postgres", "--describe-config")
	if !res.Status.Exited() || res.Status.ExitCode() != 0 {
		t.Fatalf("status %#x, stderr %q", res.Status, res.Stderr)
	}
	if n := strings.Count(res.Stdout, "\n"); n < 300 {
		t.Errorf("described %d settings, want at least 300", n)
	}
	if !strings.Contains(res.Stdout, "shared_buffers\t") {
		t.Error("shared_buffers is missing from --describe-config")
	}
}

func TestInitdbVersion(t *testing.T) {
	res := wasmrttest.Run(t, readBuild(t, "initdb.wasm"), wasmrttest.Options{}, "/pgmem/bin/initdb", "--version")
	want := "initdb (PostgreSQL) " + pinnedPostgresVersion(t) + "\n"
	if !res.Status.Exited() || res.Status.ExitCode() != 0 || res.Stdout != want {
		t.Fatalf("status %#x, stdout %q, want %q, stderr %q", res.Status, res.Stdout, want, res.Stderr)
	}
}

// checkInitialMemoryBelowShmemAddr checks that PG_SHMEM_ADDR (0x2000000,
// 32 MiB) lies above everything a process's heap holds when it attaches
// the main segment. The heap starts at the module's initial memory, so
// that must stay well below. 24 MiB leaves room for what startup
// allocates, which we measured at 10.75 MiB.
func checkInitialMemoryBelowShmemAddr(t *testing.T, m wazero.CompiledModule) {
	t.Helper()
	mems := m.ImportedMemories()
	for _, d := range m.ExportedMemories() {
		mems = append(mems, d)
	}
	if len(mems) == 0 {
		t.Error("postgres.wasm has no memory")
		return
	}
	if min := uint64(mems[0].Min()) * 65536; min > 24<<20 {
		t.Errorf("postgres.wasm initial memory %d bytes, want <= 24 MiB", min)
	}
}
