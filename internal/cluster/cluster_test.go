package cluster_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/cluster"
	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// loadBuild loads make wasm's output from $PGMEM_BUILD_OUT, else
// $PGMEM_BUILD_DIR, else this module's build/out. Without
// PGMEM_REQUIRE_BUILD a missing build skips the test.
func loadBuild(t *testing.T) cluster.Build {
	t.Helper()
	dir := os.Getenv("PGMEM_BUILD_OUT")
	if dir == "" {
		dir = os.Getenv("PGMEM_BUILD_DIR")
	}
	if dir == "" {
		dir = filepath.Join("..", "..", "build", "out")
	}
	b, err := cluster.LoadBuild(dir)
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("PGMEM_REQUIRE_BUILD") == "" {
		t.Skipf("not built in %s; run make wasm (PGMEM_BUILD_DIR must be absolute: tests run in their package directory)", dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newKernel returns an installed kernel over fsys. Cleanup closes it, with
// a time bound.
func newKernel(t *testing.T, fsys *memfs.FS, b cluster.Build) *kernel.Kernel {
	t.Helper()
	return newKernelConfig(t, fsys, b, kernel.Config{})
}

// newKernelConfig is newKernel with cfg's options. It sets cfg's FS and
// CacheDir.
func newKernelConfig(t *testing.T, fsys *memfs.FS, b cluster.Build, cfg kernel.Config) *kernel.Kernel {
	t.Helper()
	ctx := context.Background()
	cache, err := wasmrt.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg.FS, cfg.CacheDir = fsys, cache
	k, err := kernel.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := k.Close(cctx); err != nil {
			t.Errorf("kernel close: %v", err)
		}
	})
	if err := cluster.Install(ctx, k, fsys, b); err != nil {
		t.Fatal(err)
	}
	return k
}

func single(t *testing.T, k *kernel.Kernel, sql string) (string, string) {
	t.Helper()
	out, errOut, st, err := cluster.Single(context.Background(), k, sql)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exited() || st.ExitCode() != 0 {
		t.Fatalf("postgres --single: status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
	return out, errOut
}

// singleOK is single for statements that must all succeed. stderr may not
// report an ERROR.
func singleOK(t *testing.T, k *kernel.Kernel, sql string) (string, string) {
	t.Helper()
	out, errOut := single(t, k, sql)
	if strings.Contains(errOut, "ERROR:") {
		t.Fatalf("postgres --single reported an error\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	return out, errOut
}

// Install without initdb, as pgmem.Start calls it, neither writes nor
// registers initdb. It installs postgres as usual.
func TestInstallWithoutInitdb(t *testing.T) {
	b := loadBuild(t)
	b.Initdb = nil
	fsys := memfs.New()
	k := newKernel(t, fsys, b)
	if _, err := fsys.ReadFile(cluster.PostgresPath); err != nil {
		t.Fatalf("postgres not installed: %v", err)
	}
	if _, err := fsys.ReadFile("/pgmem/bin/initdb"); err == nil {
		t.Fatal("initdb's program file was written")
	}
	if _, err := k.Start(context.Background(), kernel.Spec{Path: "/pgmem/bin/initdb"}); err == nil || !strings.Contains(err.Error(), "not a registered program") {
		t.Fatalf("starting initdb: %v, want not registered", err)
	}
}

func TestCreateAndQuery(t *testing.T) {
	b := loadBuild(t)
	fsys := memfs.New()
	k := newKernel(t, fsys, b)
	var log bytes.Buffer
	if err := cluster.Create(context.Background(), k, fsys, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "Success.") {
		t.Fatalf("initdb did not report success:\n%s", log.String())
	}
	if hba, err := fsys.ReadFile(cluster.DataDir + "/pg_hba.conf"); err != nil || string(hba) != cluster.HBA {
		t.Fatalf("pg_hba.conf = %q, %v; want %q", hba, err, cluster.HBA)
	}

	t.Run("select", func(t *testing.T) {
		out, _ := singleOK(t, k, "SELECT 1;\n")
		if !strings.Contains(out, `?column? = "1"`) {
			t.Fatalf("stdout:\n%s", out)
		}
	})
	t.Run("settings", func(t *testing.T) {
		// Build the SHOW list and the expected values from cluster.Settings so
		// every baked setting is checked. SHOW prints the GUC name in its
		// canonical case (TimeZone) and normalises values, so names match
		// case-insensitively.
		var sql strings.Builder
		var names, wants []string
		for _, kv := range cluster.Settings {
			name, val, _ := strings.Cut(kv, " = ")
			val = strings.Trim(val, "'")
			// Every configured value is already in SHOW's normalised form
			// ("32MB", "off", "sysv", "UTC", ""), so no per-setting exceptions.
			names = append(names, name)
			wants = append(wants, fmt.Sprintf("%s = %q", name, val))
			fmt.Fprintf(&sql, "SHOW %s;\n", name)
		}
		out, _ := singleOK(t, k, sql.String())
		lower := strings.ToLower(out)
		for i, want := range wants {
			if !strings.Contains(lower, strings.ToLower(want)) {
				t.Errorf("setting %s: missing %s in:\n%s", names[i], want, out)
			}
		}
	})
	t.Run("plpgsql", func(t *testing.T) {
		// Single-user mode logs only at log_min_messages and above, which
		// defaults to warning. The test builds the notice text at run time,
		// so the statement echoed after an ERROR cannot match it.
		_, errOut := singleOK(t, k, "SET log_min_messages = notice;\nDO $$BEGIN RAISE NOTICE 'plpgsql %', 'works'; END$$;\n")
		if !strings.Contains(errOut, "NOTICE:  plpgsql works") {
			t.Fatalf("stderr:\n%s", errOut)
		}
	})
	t.Run("snowball", func(t *testing.T) {
		out, _ := singleOK(t, k, "SELECT to_tsvector('english', 'running dogs');\n")
		if !strings.Contains(out, "'dog':2 'run':1") {
			t.Fatalf("stdout:\n%s", out)
		}
	})
	t.Run("btree_gist", func(t *testing.T) {
		_, errOut := single(t, k, strings.Join([]string{
			"CREATE EXTENSION btree_gist;",
			"CREATE TABLE r (id int, during tsrange, EXCLUDE USING gist (id WITH =, during WITH &&));",
			"INSERT INTO r VALUES (1, '[2020-01-01,2020-01-02)');",
			"INSERT INTO r VALUES (1, '[2020-01-01 12:00,2020-01-03)');",
		}, "\n")+"\n")
		if !strings.Contains(errOut, "conflicting key value violates exclusion constraint") {
			t.Fatalf("exclusion constraint did not fire:\n%s", errOut)
		}
		if n := strings.Count(errOut, "ERROR:"); n != 1 {
			t.Fatalf("want only the exclusion violation, got %d errors:\n%s", n, errOut)
		}
	})

	t.Run("image round trip", func(t *testing.T) {
		var img bytes.Buffer
		if err := fsys.WriteTar(&img, cluster.DataDir); err != nil {
			t.Fatal(err)
		}
		fresh := memfs.New()
		if err := fresh.LoadTar(&img, cluster.DataDir); err != nil {
			t.Fatal(err)
		}
		k2 := newKernel(t, fresh, b)
		out, _ := singleOK(t, k2, "SELECT count(*) FROM r;\n")
		if !strings.Contains(out, `count = "1"`) {
			t.Fatalf("reloaded image lost data:\n%s", out)
		}
	})
}

// Two clusters made with the deterministic kernel are byte-identical, so
// the committed cluster image can be checked against its sources. The test
// makes each one as mkcluster does, with initdb and then a query. The
// second one's backend waits idle longer than any Postgres timer it arms,
// which is the 10s idle stats update, as on a slow host. The image may not
// depend on host speed.
//
// The stalled backend stands in for the real case. On the slow, emulated
// host where the image changed, initdb's post-bootstrap backend saw the
// timer fire. Nothing else arms a timer there, and the guard is the same:
// timers never fire in a deterministic kernel. One machine cannot check
// independence from the host's architecture, which the interpreter
// provides. The test builds the two images one after the other, because
// tests run one Postgres at a time.
func TestCreateDeterministic(t *testing.T) {
	if raceEnabled {
		t.Skip("two interpreted initdb runs take minutes under the race detector")
	}
	b := loadBuild(t)
	img := func(pause time.Duration) []byte {
		fsys := memfs.New()
		k := newKernelConfig(t, fsys, b, kernel.Config{Deterministic: true})
		var log bytes.Buffer
		if err := cluster.Create(context.Background(), k, fsys, &log); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		var out, errOut bytes.Buffer
		p, err := k.Start(context.Background(), kernel.Spec{
			Path:   cluster.PostgresPath,
			Args:   []string{"--single", "-D", cluster.DataDir, "postgres"},
			Stdin:  &pausingReader{parts: []string{"SELECT 1;\n", "SELECT 2;\n"}, pause: pause},
			Stdout: &out,
			Stderr: &errOut,
		})
		if err != nil {
			t.Fatal(err)
		}
		if st := p.Wait(); !st.Exited() || st.ExitCode() != 0 || !strings.Contains(out.String(), `?column? = "2"`) {
			t.Fatalf("postgres --single: status %#x\nstdout:\n%s\nstderr:\n%s", st, out.String(), errOut.String())
		}
		var buf bytes.Buffer
		if err := fsys.WriteTar(&buf, cluster.DataDir); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	a, b2 := img(0), img(10500*time.Millisecond)
	if !bytes.Equal(a, b2) {
		t.Fatalf("images differ: %s", firstDiff(a, b2))
	}
}

// pausingReader returns one part per Read, sleeping pause before every
// part but the first. It acts as a client that leaves the backend idle in
// between.
type pausingReader struct {
	parts []string
	pause time.Duration
	n     int // index of the current part
	off   int // bytes of it already read
}

func (r *pausingReader) Read(p []byte) (int, error) {
	if r.n >= len(r.parts) {
		return 0, io.EOF
	}
	if r.n > 0 && r.off == 0 {
		time.Sleep(r.pause)
	}
	n := copy(p, r.parts[r.n][r.off:])
	if r.off += n; r.off == len(r.parts[r.n]) {
		r.n, r.off = r.n+1, 0
	}
	return n, nil
}

// firstDiff names the first tar entry whose header or content differs
// between the images a and b, or the entry one has where the other ends.
func firstDiff(a, b []byte) string {
	ra, rb := tar.NewReader(bytes.NewReader(a)), tar.NewReader(bytes.NewReader(b))
	for i := 0; ; i++ {
		ha, errA := ra.Next()
		hb, errB := rb.Next()
		switch {
		case errA == io.EOF && errB == io.EOF:
			return "same entries; the tar framing differs"
		case errA != nil && errA != io.EOF || errB != nil && errB != io.EOF:
			return fmt.Sprintf("reading entry %d: %v / %v", i, errA, errB)
		case errA == io.EOF:
			return fmt.Sprintf("the first image ends after %d entries; the second has %q next", i, hb.Name)
		case errB == io.EOF:
			return fmt.Sprintf("the second image ends after %d entries; the first has %q next", i, ha.Name)
		case ha.Name != hb.Name:
			return fmt.Sprintf("entry %d: %q vs %q", i, ha.Name, hb.Name)
		case !reflect.DeepEqual(ha, hb):
			return fmt.Sprintf("%s: header differs:\n%+v\n%+v", ha.Name, *ha, *hb)
		}
		ca, errA := io.ReadAll(ra)
		cb, errB := io.ReadAll(rb)
		if errA != nil || errB != nil {
			return fmt.Sprintf("%s: reading content: %v / %v", ha.Name, errA, errB)
		}
		if !bytes.Equal(ca, cb) {
			j := 0
			for j < len(ca) && j < len(cb) && ca[j] == cb[j] {
				j++
			}
			return fmt.Sprintf("%s: content differs from byte %d", ha.Name, j)
		}
	}
}

// firstDiff names the entry that differs by content or metadata, or the
// entry where one image ends first.
func TestFirstDiff(t *testing.T) {
	type entry struct {
		name, body string
		uid        int
	}
	image := func(entries ...entry) []byte {
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		for _, e := range entries {
			if err := w.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Uid: e.uid, Format: tar.FormatPAX}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	x, y := entry{"x", "abc", 0}, entry{"y", "def", 0}
	for _, c := range []struct {
		a, b []byte
		want string
	}{
		{image(x, y), image(x, entry{"y", "dXf", 0}), "y: content differs from byte 1"},
		{image(x, y), image(x, entry{"y", "def", 7}), "y: header differs"},
		{image(x), image(x, y), `the first image ends after 1 entries; the second has "y" next`},
		{image(x, y), image(x), `the second image ends after 1 entries; the first has "y" next`},
		{image(x, y), image(y, x), `entry 0: "x" vs "y"`},
	} {
		if got := firstDiff(c.a, c.b); !strings.HasPrefix(got, c.want) {
			t.Errorf("firstDiff = %q, want prefix %q", got, c.want)
		}
	}
}
