package pgmem_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/klauspost/compress/zstd"

	"github.com/feverenergy/pgmem"
	"github.com/feverenergy/pgmem/internal/buildid"
)

// testEngine is Postgres17 under another Version. It has the same files,
// so it boots, but pgmem must treat it as an engine of its own.
func testEngine() pgmem.Engine {
	e := pgmem.Postgres17
	e.Version = "17.11-test"
	return e
}

// TestStartWithEngine starts Postgres17 by name, with PGMEM_BUILD_DIR
// unset, and checks that the server reports the engine's Version.
func TestStartWithEngine(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "")
	s := start(t, pgmem.Options{Engine: pgmem.Postgres17})
	var version string
	if err := connect(t, s).QueryRow(context.Background(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != pgmem.Postgres17.Version {
		t.Fatalf("server_version %q, want %q", version, pgmem.Postgres17.Version)
	}
}

// Each engine is decompressed once per process. Every load of an engine
// returns the same slices, so all its servers share one copy. The zero
// Engine is Postgres17. An engine with another Version gets its own copy
// and its own build ID, even when its files are the same.
func TestEnginesDecompressedOnce(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "")
	magic := []struct {
		name  string
		magic []byte
		off   int
	}{
		{"postgres.wasm", []byte("\x00asm"), 0},
		{"share.tar", []byte("ustar"), 257},
		{"cluster.tar", []byte("ustar"), 257},
	}
	load := func(e pgmem.Engine) [3][]byte {
		t.Helper()
		p, s, c, err := pgmem.EngineFiles(e)
		if err != nil {
			t.Fatalf("engine %q: %v", e.Version, err)
		}
		files := [3][]byte{p, s, c}
		for i, m := range magic {
			f := files[i]
			if len(f) < m.off+len(m.magic) || !bytes.Equal(f[m.off:m.off+len(m.magic)], m.magic) {
				t.Fatalf("engine %q: %s: %d bytes without the %q magic", e.Version, m.name, len(f), m.magic)
			}
		}
		return files
	}
	same := func(a, b [3][]byte) bool {
		for i := range a {
			if &a[i][0] != &b[i][0] || len(a[i]) != len(b[i]) {
				return false
			}
		}
		return true
	}
	pg17, other := load(pgmem.Postgres17), load(testEngine())
	if !same(pg17, load(pgmem.Postgres17)) {
		t.Fatal("a second load of Postgres17 returned other slices")
	}
	if !same(pg17, load(pgmem.Engine{})) {
		t.Fatal("the zero Engine did not return Postgres17's slices")
	}
	if !same(other, load(testEngine())) {
		t.Fatal("a second load of the test engine returned other slices")
	}
	for i := range pg17 {
		if &pg17[i][0] == &other[i][0] {
			t.Fatalf("%s: the test engine shares Postgres17's slice", magic[i].name)
		}
		if !bytes.Equal(pg17[i], other[i]) {
			t.Fatalf("%s: the test engine's bytes differ from Postgres17's", magic[i].name)
		}
	}

	id := func(e pgmem.Engine) string {
		t.Helper()
		id, err := buildid.ID(e.Version, e.FS)
		if err != nil {
			t.Fatalf("build ID of engine %q: %v", e.Version, err)
		}
		return id
	}
	if a, b := id(pgmem.Postgres17), id(testEngine()); a == b {
		t.Fatalf("Postgres17 and the test engine have the same build ID %s", a)
	}
	if a, b := id(pgmem.Postgres17), id(pgmem.Engine{}); a != b {
		t.Fatalf("the zero Engine's build ID %s differs from Postgres17's %s", b, a)
	}
}

// zst compresses data with zstd.
func zst(t *testing.T, data string) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll([]byte(data), nil)
}

// An engine with a missing or corrupt file fails Start and the build ID
// with an error that names the engine's Version and the file. The fake
// files are tiny, so the test decompresses no real build.
func TestEngineBadFiles(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "")
	files := func(cluster []byte) fstest.MapFS {
		m := fstest.MapFS{
			"postgres.wasm.zst": {Data: zst(t, "not a real postgres.wasm")},
			"share.tar.zst":     {Data: zst(t, "not a real share.tar")},
		}
		if cluster != nil {
			m["cluster.tar.zst"] = &fstest.MapFile{Data: cluster}
		}
		return m
	}
	for _, tc := range []struct {
		name     string
		engine   pgmem.Engine
		want     []string
		notExist bool
	}{
		{"missing", pgmem.Engine{Version: "17.11-missing", FS: files(nil)}, []string{`"17.11-missing"`, "cluster.tar.zst"}, true},
		{"corrupt", pgmem.Engine{Version: "17.11-corrupt", FS: files([]byte("not zstd"))}, []string{`"17.11-corrupt"`, "cluster.tar.zst"}, false},
		{"no FS", pgmem.Engine{Version: "17.11-nofs"}, []string{`"17.11-nofs"`, "no FS"}, false},
		{"no Version", pgmem.Engine{FS: files(nil)}, []string{"no Version"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(what string, err error) {
				t.Helper()
				if err == nil {
					t.Fatalf("%s: no error", what)
				}
				t.Logf("%s: %v", what, err)
				for _, w := range tc.want {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("%s: %q does not name %s", what, err, w)
					}
				}
				if got := errors.Is(err, fs.ErrNotExist); got != tc.notExist {
					t.Errorf("%s: errors.Is(%q, fs.ErrNotExist) = %v, want %v", what, err, got, tc.notExist)
				}
			}
			check("Start", startFails(pgmem.Start(context.Background(), pgmem.Options{Engine: tc.engine})))
			_, err := buildid.ID(tc.engine.Version, tc.engine.FS)
			check("build ID", err)
		})
	}
}

// An Engine whose Version is in use with another FS fails Start and the
// build ID. pgmem cannot compare two fstest.MapFS, so a second MapFS
// under one Version gets the first one's files, as the Engine doc says.
func TestEngineVersionInUse(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "")
	if _, _, _, err := pgmem.EngineFiles(pgmem.Postgres17); err != nil {
		t.Fatal(err)
	}
	clash := pgmem.Engine{Version: pgmem.Postgres17.Version, FS: os.DirFS(t.TempDir())}
	for what, err := range map[string]error{
		"Start":    startFails(pgmem.Start(context.Background(), pgmem.Options{Engine: clash})),
		"build ID": func() error { _, err := buildid.ID(clash.Version, clash.FS); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", clash.Version)) || !strings.Contains(err.Error(), "different FS") {
			t.Errorf("%s: got %v, want an error naming the Version and a different FS", what, err)
		}
	}

	first := fstest.MapFS{
		"postgres.wasm.zst": {Data: zst(t, "not a real postgres.wasm")},
		"share.tar.zst":     {Data: zst(t, "not a real share.tar")},
	}
	second := fstest.MapFS{
		"postgres.wasm.zst": first["postgres.wasm.zst"],
		"share.tar.zst":     first["share.tar.zst"],
		"cluster.tar.zst":   {Data: []byte("not zstd")},
	}
	_, _, _, err := pgmem.EngineFiles(pgmem.Engine{Version: "17.11-maps", FS: first})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("first MapFS: got %v, want a missing cluster.tar.zst", err)
	}
	_, _, _, err = pgmem.EngineFiles(pgmem.Engine{Version: "17.11-maps", FS: second})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("second MapFS: got %v, want the first one's missing cluster.tar.zst", err)
	}
}

// PGMEM_BUILD_DIR replaces every engine, including one whose files are
// missing. All engines then have the directory's build ID.
func TestBuildDirOverridesEngine(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"postgres.wasm", "share.tar", "cluster.tar"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PGMEM_BUILD_DIR", dir)
	missing := pgmem.Engine{Version: "17.11-missing", FS: fstest.MapFS{}}
	want, err := buildid.ID(pgmem.Postgres17.Version, pgmem.Postgres17.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []pgmem.Engine{{}, testEngine(), missing} {
		if id, err := buildid.ID(e.Version, e.FS); err != nil || id != want {
			t.Errorf("build ID of engine %q: %q %v, want the directory's %q", e.Version, id, err, want)
		}
	}
}

// localBuild returns the local build directory TestDefaultEngineMatchesBuild
// compares against. That is $PGMEM_BUILD_OUT, else $PGMEM_BUILD_DIR, else
// this module's build/out.
func localBuild() string {
	for _, v := range []string{"PGMEM_BUILD_OUT", "PGMEM_BUILD_DIR"} {
		if dir := os.Getenv(v); dir != "" {
			return dir
		}
	}
	return filepath.Join("build", "out")
}

// The default engine's files are the local build's. After make wasm, as
// make check runs it, a postgres.wasm or share.tar that differs from the
// engine's means the engine module is stale. The test compares cluster.tar
// when the local build has one, from make cluster. Without a local build
// the test skips, unless PGMEM_REQUIRE_BUILD is set.
func TestDefaultEngineMatchesBuild(t *testing.T) {
	dir := localBuild()
	postgres, share, cluster, err := pgmem.EngineFiles(pgmem.Postgres17)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		file     string
		engine   []byte
		optional bool // compared only when the build has it
	}{
		{"postgres.wasm", postgres, false},
		{"share.tar", share, false},
		{"cluster.tar", cluster, true},
	} {
		built, err := os.ReadFile(filepath.Join(dir, a.file))
		if errors.Is(err, fs.ErrNotExist) {
			if a.optional {
				t.Logf("%s: not in %s; not compared (make cluster)", a.file, dir)
				continue
			}
			if os.Getenv("PGMEM_REQUIRE_BUILD") == "" {
				t.Skipf("%s is not built; run make wasm (PGMEM_BUILD_DIR must be absolute: tests run in their package directory)", filepath.Join(dir, a.file))
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.engine, built) {
			t.Errorf("engine %s is stale: run make assets (%s: engine %d bytes sha256 %x, %s has %d bytes sha256 %x)",
				pgmem.Postgres17.Version, a.file, len(a.engine), sha256.Sum256(a.engine), dir, len(built), sha256.Sum256(built))
		}
	}
}

// Postgres17's INPUTS.SHA256SUMS lists the sha256 of each decompressed
// file, sorted by name, and the files match it. INPUTS.SHA256SUMS is
// optional for an engine, but Postgres17 has one.
func TestDefaultEngineInputSums(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "")
	sums, err := fs.ReadFile(pgmem.Postgres17.FS, "INPUTS.SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	postgres, share, cluster, err := pgmem.EngineFiles(pgmem.Postgres17)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"cluster.tar": cluster, "postgres.wasm": postgres, "share.tar": share}
	var listed []string
	for _, line := range strings.SplitAfter(string(sums), "\n") {
		if line == "" {
			break
		}
		sum, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "  ")
		if !ok || files[name] == nil || !strings.HasSuffix(line, "\n") {
			t.Fatalf("INPUTS.SHA256SUMS line %q is not \"<hex>  <file>\\n\"", line)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(files[name])); got != sum {
			t.Errorf("engine %s: %s: decompressed sha256 %s, INPUTS.SHA256SUMS says %s (run make assets)",
				pgmem.Postgres17.Version, name, got, sum)
		}
		listed = append(listed, name)
	}
	if want := []string{"cluster.tar", "postgres.wasm", "share.tar"}; !slices.Equal(listed, want) {
		t.Fatalf("INPUTS.SHA256SUMS lists %q, want %q (sorted)", listed, want)
	}
}
