package pgtest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem/internal/buildid"
)

// useCacheDir points PGTEST_CACHE_DIR at a new temporary directory, and
// skips the test as startServer does when the build is missing.
func useCacheDir(t *testing.T) string {
	t.Helper()
	requireBuild(t)
	dir := t.TempDir()
	t.Setenv("PGTEST_CACHE_DIR", dir)
	captureCacheWarning(t)
	return dir
}

// captureCacheWarning sends the process's cache warning (cacheStderr) to a
// buffer, and lets it be written again, for one test.
func captureCacheWarning(t *testing.T) *syncBuffer {
	t.Helper()
	var b syncBuffer
	old := cacheStderr
	cacheStderr = &b
	cacheWarned.Store(false)
	t.Cleanup(func() {
		cacheStderr = old
		cacheWarned.Store(false)
	})
	return &b
}

// requireBuild skips the test when PGMEM_BUILD_DIR names an incomplete
// local build, unless PGMEM_REQUIRE_BUILD is set.
func requireBuild(t *testing.T) {
	t.Helper()
	_, err := buildid.ID("", nil)
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("PGMEM_BUILD_DIR") != "" && os.Getenv("PGMEM_REQUIRE_BUILD") == "" {
		t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err != nil {
		t.Fatal(err)
	}
}

// imagePath is where the image for key, on the default engine, must be
// cached in dir.
func imagePath(t *testing.T, dir, key string) string {
	t.Helper()
	return templatePath(t, dir, Template{Key: key})
}

// templatePath is where the image for tpl must be cached in dir, computed
// independently of cache.go.
func templatePath(t *testing.T, dir string, tpl Template) string {
	t.Helper()
	id, err := buildid.ID(tpl.Engine.Version, tpl.Engine.FS)
	if err != nil {
		t.Fatal(err)
	}
	support := sha256.Sum256([]byte(supportSQL))
	sum := sha256.Sum256([]byte(string(support[:]) + "\x00" + "2" + "\x00" + tpl.Key))
	return filepath.Join(dir, id[:16]+"-"+hex.EncodeToString(sum[:])[:16]+".img")
}

// countingTemplate is a template whose Migrate counts its calls, checks it
// runs in database tmpl and creates a table there.
func countingTemplate(key string, calls *atomic.Int32) Template {
	return Template{Key: key, Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		calls.Add(1)
		var db string
		if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
			return err
		}
		if db != "tmpl" {
			return fmt.Errorf("Migrate runs in %q, want tmpl", db)
		}
		_, err := pool.Exec(ctx, "CREATE TABLE migrated (id int PRIMARY KEY); INSERT INTO migrated VALUES (1)")
		return err
	}}
}

// buildImage calls templateImage, failing the test on an error, and
// returns the image and the lines logf received.
func buildImage(t *testing.T, tpl Template) ([]byte, []string) {
	t.Helper()
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	start := time.Now()
	img, err := templateImage(context.Background(), tpl, logf)
	if err != nil {
		t.Fatalf("templateImage: %v", err)
	}
	t.Logf("templateImage(%q): %d bytes (%.1f MB) in %v; logf: %q", tpl.Key, len(img), float64(len(img))/(1<<20), time.Since(start), lines)
	return img, lines
}

// checkImage checks that img is a tar of a cleanly shut down data
// directory.
func checkImage(t *testing.T, img []byte) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(img))
	var version, pid bool
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("image is not a tar (after %d entries): %v", n, err)
		}
		n++
		switch path.Base(h.Name) {
		case "PG_VERSION":
			version = true
		case "postmaster.pid":
			pid = true
		}
	}
	if !version || pid {
		t.Fatalf("image (%d entries): PG_VERSION %v, postmaster.pid %v; want true, false", n, version, pid)
	}
}

// cacheFiles lists the names in dir.
func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCacheDir(t *testing.T) {
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory: %v", err)
	}
	for _, tc := range []struct{ env, want string }{
		{"", filepath.Join(userCache, "pgmem", "snapshots")},
		{"off", ""},
		{"/some/dir", "/some/dir"},
	} {
		t.Setenv("PGTEST_CACHE_DIR", tc.env)
		if got, err := cacheDir(); err != nil || got != tc.want {
			t.Errorf("PGTEST_CACHE_DIR=%q: cacheDir() = %q, %v; want %q", tc.env, got, err, tc.want)
		}
	}

	// A relative directory would be a different one in each package's
	// test binary, because go test runs it in the package's directory.
	t.Setenv("PGTEST_CACHE_DIR", "some/dir")
	if got, err := cacheDir(); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Errorf("PGTEST_CACHE_DIR=some/dir: cacheDir() = %q, %v; want an error saying it is not absolute", got, err)
	}

	// With no user cache directory, the default fails.
	t.Setenv("PGTEST_CACHE_DIR", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if got, err := cacheDir(); err == nil {
		t.Errorf("with no HOME: cacheDir() = %q, nil; want an error", got)
	}
}

// TestCacheNameCoversSupport checks that the file name changes with the
// support schema, the template layout version and the Key, and with
// nothing else.
func TestCacheNameCoversSupport(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := cacheFileName(id, "support", "1", "key")
	if ok, _ := regexp.MatchString(`^0123456789abcdef-[0-9a-f]{16}\.img$`, base); !ok {
		t.Fatalf("cacheFileName = %q, want <buildID[:16]>-<16 hex>.img", base)
	}
	if again := cacheFileName(id, "support", "1", "key"); again != base {
		t.Fatalf("cacheFileName is not stable: %q, %q", base, again)
	}
	for _, other := range []string{
		cacheFileName(id, "support changed", "1", "key"),
		cacheFileName(id, "support", "2", "key"),
		cacheFileName(id, "support", "1", "key2"),
		cacheFileName(strings.Repeat("f", 64), "support", "1", "key"),
	} {
		if other == base {
			t.Errorf("a different input gives the same name %q", base)
		}
	}
}

func TestTemplateImageCachedOnce(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	tpl := countingTemplate("cached-once", &calls)

	first, lines := buildImage(t, tpl)
	checkImage(t, first)
	if len(lines) != 1 || !strings.Contains(lines[0], "built") {
		t.Errorf("first call logged %q, want one line saying it built", lines)
	}
	path := imagePath(t, dir, tpl.Key)
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cached image: %v (dir holds %q)", err, cacheFiles(t, dir))
	}
	if !bytes.Equal(onDisk, first) {
		t.Fatal("the cached file differs from the image returned")
	}

	second, lines := buildImage(t, tpl)
	if !bytes.Equal(second, first) {
		t.Fatal("the second call returned a different image")
	}
	if len(lines) != 0 {
		t.Errorf("reading the cache logged %q", lines)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("Migrate ran %d times, want 1", n)
	}
	// The image and its lock file, which is never deleted. No temporary file.
	want := []string{filepath.Base(path), filepath.Base(path) + ".lock"}
	if got := cacheFiles(t, dir); !equalSets(got, want) {
		t.Fatalf("cache dir holds %q, want %q", got, want)
	}
}

func TestTemplateKeyChangeRebuilds(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	buildImage(t, countingTemplate("key-a", &calls))
	buildImage(t, countingTemplate("key-b", &calls))
	if n := calls.Load(); n != 2 {
		t.Fatalf("Migrate ran %d times, want 2", n)
	}
	a, b := imagePath(t, dir, "key-a"), imagePath(t, dir, "key-b")
	if a == b {
		t.Fatalf("keys a and b share the file name %s", a)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%v (dir holds %q)", err, cacheFiles(t, dir))
		}
	}
}

func TestMigrateErrorNotCached(t *testing.T) {
	dir := useCacheDir(t)
	boom := errors.New("boom: migration 7 failed")
	tpl := Template{Key: "failing", Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		return boom
	}}
	img, err := templateImage(context.Background(), tpl, t.Logf)
	if err == nil || !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "pgtest: migrate: ") {
		t.Fatalf("templateImage = %d bytes, %v; want an error wrapping %q, starting \"pgtest: migrate: \"", len(img), err, boom)
	}
	// The lock file, and the failure marker for processes that waited.
	path := imagePath(t, dir, tpl.Key)
	want := []string{filepath.Base(path) + ".lock", filepath.Base(path) + ".failed"}
	if got := cacheFiles(t, dir); !equalSets(got, want) {
		t.Fatalf("cache dir holds %q after a failed build, want %q", got, want)
	}
	if text, rerr := os.ReadFile(path + ".failed"); rerr != nil || string(text) != err.Error() {
		t.Fatalf("failure marker %q, %v; want the error's text %q", text, rerr, err)
	}
}

// runBuild calls templateImage in a goroutine of its own, so that Migrate
// can end it with runtime.Goexit, and recovers a panic. It fails the test
// if templateImage has not returned within a minute.
func runBuild(t *testing.T, tpl Template) (img []byte, err error, panicked any) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		img, err = templateImage(context.Background(), tpl, t.Logf)
	}()
	select {
	case <-done:
		return img, err, panicked
	case <-time.After(time.Minute):
		t.Fatalf("templateImage(%q) still running after a minute", tpl.Key)
		return nil, nil, nil
	}
}

// serverDial captures the build server's Dial from Migrate's pool. The
// pool dials through srv.Dial (poolConfig), which returns net.ErrClosed
// once the server is closed or closing.
type serverDial struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d *serverDial) capture(pool *pgxpool.Pool) { d.dial = pool.Config().ConnConfig.DialFunc }

// checkClosed checks that the server Migrate ran on is closed.
func (d *serverDial) checkClosed(t *testing.T) {
	t.Helper()
	if d.dial == nil {
		t.Fatal("Migrate did not run")
	}
	conn, err := d.dial(context.Background(), "unix", "/tmp/.s.PGSQL.5432")
	if err == nil {
		conn.Close()
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("after templateImage returned, the build server's Dial = %v; want net.ErrClosed (the server leaked)", err)
	}
}

// TestMigrateLeakedConnection checks that a Migrate that leaves a
// connection acquired (rows not closed) fails the build at once, and that
// the build closes the server. pgxpool's Close would otherwise hang while
// the build holds the lock.
func TestMigrateLeakedConnection(t *testing.T) {
	dir := useCacheDir(t)
	var srv serverDial
	tpl := Template{Key: "leaky", Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		srv.capture(pool)
		_, err := pool.Query(ctx, "SELECT 1") // rows never closed
		return err
	}}
	start := time.Now()
	_, err, panicked := runBuild(t, tpl)
	if panicked != nil {
		t.Fatalf("panic: %v", panicked)
	}
	if err == nil || !strings.Contains(err.Error(), "pgtest: migrate: Migrate left 1 connection(s) acquired") {
		t.Fatalf("templateImage: %v; want the leaked connection reported", err)
	}
	t.Logf("failed in %v: %v", time.Since(start), err)
	srv.checkClosed(t)
	if _, err := os.Stat(imagePath(t, dir, tpl.Key)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("image cached after a failed build: %v", err)
	}
}

// TestMigrateAbortClosesServer checks that the build closes its server and
// releases the lock when Migrate ends its goroutine or panics. t.Fatal and
// require in Migrate end it with Goexit. A build of the same Key then
// succeeds.
func TestMigrateAbortClosesServer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		abort func()
	}{
		{"goexit", runtime.Goexit},
		{"panic", func() { panic("migration exploded") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := useCacheDir(t)
			var srv serverDial
			tpl := Template{Key: "abort", Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
				srv.capture(pool)
				if _, err := pool.Exec(ctx, "CREATE TABLE half (id int)"); err != nil {
					return err
				}
				tc.abort()
				return nil
			}}
			img, err, panicked := runBuild(t, tpl)
			if img != nil || err != nil || (panicked != nil) != (tc.name == "panic") {
				t.Fatalf("templateImage = %d bytes, %v, panic %v", len(img), err, panicked)
			}
			srv.checkClosed(t)
			if _, err := os.Stat(imagePath(t, dir, tpl.Key)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("image cached after an aborted build: %v", err)
			}

			var calls atomic.Int32
			img, err, panicked = runBuild(t, countingTemplate(tpl.Key, &calls))
			if err != nil || panicked != nil || calls.Load() != 1 {
				t.Fatalf("the build after the abort: %v, panic %v, %d migrations", err, panicked, calls.Load())
			}
			checkImage(t, img)
		})
	}
}

// TestStaleTempFilesRemoved checks that the next build removes the
// temporary files left by a builder that was killed before its rename.
func TestStaleTempFilesRemoved(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	tpl := countingTemplate("stale", &calls)
	path := imagePath(t, dir, tpl.Key)
	unrelated := filepath.Join(dir, "other.img.tmp-1")
	for _, name := range []string{path + ".tmp-1", path + ".tmp-99999", unrelated} {
		if err := os.WriteFile(name, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	buildImage(t, tpl)
	want := []string{filepath.Base(path), filepath.Base(path) + ".lock", filepath.Base(unrelated)}
	if got := cacheFiles(t, dir); !equalSets(got, want) {
		t.Fatalf("cache dir holds %q, want %q", got, want)
	}
}

func TestReadOnlyCacheDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	dir := useCacheDir(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	var calls atomic.Int32
	img, lines := buildImage(t, countingTemplate("read-only", &calls))
	checkImage(t, img)
	if len(lines) != 1 || !strings.Contains(lines[0], "unusable") || !strings.Contains(lines[0], dir) {
		t.Fatalf("logf received %q, want one line saying cache %s is unusable", lines, dir)
	}
	if got := cacheFiles(t, dir); len(got) != 0 {
		t.Fatalf("read-only cache dir holds %q", got)
	}
}

func TestCorruptCacheFileRebuilds(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	tpl := countingTemplate("corrupt", &calls)
	path := imagePath(t, dir, tpl.Key)
	garbage := []byte("this is not a tar file")
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	// templateImage does not validate the file. Its caller boots it.
	got, _ := buildImage(t, tpl)
	if !bytes.Equal(got, garbage) || calls.Load() != 0 {
		t.Fatalf("templateImage did not return the cached file (%d bytes, %d builds)", len(got), calls.Load())
	}

	if err := discardCached(tpl); err != nil {
		t.Fatalf("discardCached: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after discardCached: stat %s: %v", path, err)
	}
	if err := discardCached(tpl); err != nil {
		t.Fatalf("discardCached with no file: %v", err)
	}

	img, _ := buildImage(t, tpl)
	checkImage(t, img)
	if n := calls.Load(); n != 1 {
		t.Fatalf("Migrate ran %d times, want 1", n)
	}
	if onDisk, err := os.ReadFile(path); err != nil || !bytes.Equal(onDisk, img) {
		t.Fatalf("the rebuilt image was not cached (%d bytes, %v)", len(onDisk), err)
	}
}

func TestCacheOff(t *testing.T) {
	requireBuild(t)
	t.Setenv("PGTEST_CACHE_DIR", "off")
	wd := t.TempDir()
	t.Chdir(wd) // so that a cache in a directory named "off" would show
	var calls atomic.Int32
	img, lines := buildImage(t, countingTemplate("off", &calls))
	checkImage(t, img)
	if len(lines) != 1 || !strings.Contains(lines[0], "built") {
		t.Errorf("logf received %q, want one line saying it built", lines)
	}
	if got := cacheFiles(t, wd); len(got) != 0 {
		t.Fatalf("with the cache off, the working directory holds %q", got)
	}
}

// TestTwoProcessesRaceToBuild runs two copies of the test binary that
// build the same template at once (TestHelperBuildTemplate). One builds and
// caches it; the other waits on the lock and reads the file. This process
// starts no server.
func TestTwoProcessesRaceToBuild(t *testing.T) {
	dir := useCacheDir(t)
	counter := filepath.Join(t.TempDir(), "migrations")
	outs := runHelpers(t, "^TestHelperBuildTemplate$", "PGTEST_HELPER_COUNTER="+counter)
	sums := make([]string, len(outs))
	builders := 0
	for i, out := range outs {
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			line := sc.Text()
			if s, ok := strings.CutPrefix(line, "image sha256 "); ok {
				sums[i] = s
			}
			if strings.HasPrefix(line, "logf: ") && strings.Contains(line, "built") {
				builders++
			}
		}
	}
	if sums[0] == "" || sums[0] != sums[1] {
		t.Fatalf("children report images %q and %q, want one and the same", sums[0], sums[1])
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 {
		t.Fatalf("Migrate ran %d times (%q), want once", len(lines), lines)
	}
	if builders != 1 {
		t.Fatalf("%d children logged a build, want 1", builders)
	}
	img, err := os.ReadFile(imagePath(t, dir, "race"))
	if err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprintf("%x", sha256.Sum256(img)); s != sums[0] {
		t.Fatalf("cached image sha256 %s, children reported %s", s, sums[0])
	}
}

// runHelpers runs two copies of the test binary at once, each running the
// tests that run matches with PGTEST_HELPER=1 and env added, and returns
// their stdout. A child that fails fails the test.
func runHelpers(t *testing.T, run string, env ...string) []string {
	t.Helper()
	type child struct {
		cmd            *exec.Cmd
		stdout, stderr bytes.Buffer
	}
	children := make([]*child, 2)
	start := time.Now()
	for i := range children {
		c := &child{cmd: exec.Command(os.Args[0], "-test.run="+run, "-test.timeout=5m")}
		c.cmd.Env = append(append(os.Environ(), "PGTEST_HELPER=1"), env...)
		c.cmd.Stdout, c.cmd.Stderr = &c.stdout, &c.stderr
		if err := c.cmd.Start(); err != nil {
			for _, started := range children[:i] {
				started.cmd.Process.Kill()
				started.cmd.Wait()
			}
			t.Fatal(err)
		}
		children[i] = c
	}
	outs := make([]string, len(children))
	for i, c := range children {
		err := c.cmd.Wait()
		t.Logf("child %d (%v, %v):\n%s%s", i, err, time.Since(start), c.stdout.String(), c.stderr.String())
		if err != nil {
			t.Errorf("child %d: %v", i, err)
		}
		outs[i] = c.stdout.String()
	}
	if t.Failed() {
		t.FailNow()
	}
	return outs
}

// TestHelperBuildTemplate is the child of TestTwoProcessesRaceToBuild. Its
// Migrate sleeps 500 ms, then appends a line to $PGTEST_HELPER_COUNTER. It
// prints logf's lines and the image's sha256 on stdout.
func TestHelperBuildTemplate(t *testing.T) {
	if os.Getenv("PGTEST_HELPER") != "1" {
		t.Skip("run by TestTwoProcessesRaceToBuild")
	}
	counter := os.Getenv("PGTEST_HELPER_COUNTER")
	tpl := Template{Key: "race", Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		time.Sleep(500 * time.Millisecond)
		f, err := os.OpenFile(counter, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "migrated by pid %d\n", os.Getpid())
		if err := f.Close(); err != nil {
			return err
		}
		_, err = pool.Exec(ctx, "CREATE TABLE raced (id int)")
		return err
	}}
	logf := func(format string, args ...any) { fmt.Printf("logf: "+format+"\n", args...) }
	img, err := templateImage(context.Background(), tpl, logf)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("image sha256 %x\n", sha256.Sum256(img))
}

// TestCacheWarningOnStderr checks that pgtest reports a cache that cannot
// be used to logf every time, and once per process to stderr, which go
// test shows without -v.
func TestCacheWarningOnStderr(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	dir := useCacheDir(t)
	stderr := captureCacheWarning(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	var calls atomic.Int32
	for _, key := range []string{"warn-a", "warn-b"} {
		_, lines := buildImage(t, countingTemplate(key, &calls))
		if len(lines) != 1 || !strings.Contains(lines[0], "unusable") {
			t.Errorf("%s: logf received %q, want one line saying the cache is unusable", key, lines)
		}
	}
	out := stderr.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "pgtest: snapshot cache "+dir+" unusable") {
		t.Fatalf("stderr %q, want one line saying cache %s is unusable", out, dir)
	}
}

// TestRelativeCacheDirBuildsInMemory checks that a relative
// PGTEST_CACHE_DIR is unusable, because each package's test binary runs in
// its own directory. templateImage builds the image in memory, warns, and
// writes nothing.
func TestRelativeCacheDirBuildsInMemory(t *testing.T) {
	requireBuild(t)
	stderr := captureCacheWarning(t)
	t.Setenv("PGTEST_CACHE_DIR", "relative-cache")
	wd := t.TempDir()
	t.Chdir(wd)
	var calls atomic.Int32
	img, lines := buildImage(t, countingTemplate("relative", &calls))
	checkImage(t, img)
	if len(lines) != 1 || !strings.Contains(lines[0], "unusable") || !strings.Contains(lines[0], "not an absolute path") {
		t.Errorf("logf received %q, want one line saying the cache is unusable, as the path is not absolute", lines)
	}
	if !strings.Contains(stderr.String(), "not an absolute path") {
		t.Errorf("stderr %q, want the warning", stderr.String())
	}
	if got := cacheFiles(t, wd); len(got) != 0 {
		t.Fatalf("the working directory holds %q", got)
	}
}

// TestLockUnsupportedStillCaches checks that on a file system without
// flock (ENOLCK on NFS, or ENOTSUP), templateImage still builds the image
// and caches it with an atomic write. At worst two processes build it.
func TestLockUnsupportedStillCaches(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOLCK, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		t.Run(errno.Error(), func(t *testing.T) {
			dir := useCacheDir(t)
			old := flock
			flock = func(fd, how int) error {
				if how == syscall.LOCK_UN {
					return old(fd, how)
				}
				return errno
			}
			t.Cleanup(func() { flock = old })
			var calls atomic.Int32
			tpl := countingTemplate("unlocked", &calls)
			img, lines := buildImage(t, tpl)
			checkImage(t, img)
			if len(lines) != 1 || !strings.Contains(lines[0], "cached as") {
				t.Errorf("logf received %q, want one line saying it built and cached the image", lines)
			}
			path := imagePath(t, dir, tpl.Key)
			if onDisk, err := os.ReadFile(path); err != nil || !bytes.Equal(onDisk, img) {
				t.Fatalf("the image was not cached (%d bytes, %v; dir holds %q)", len(onDisk), err, cacheFiles(t, dir))
			}
			for _, name := range cacheFiles(t, dir) {
				if strings.Contains(name, ".tmp-") {
					t.Errorf("temporary file %s left", name)
				}
			}
		})
	}
}

// TestStaleFailureMarkerIgnored checks that templateImage ignores a
// failure marker from before this process started waiting, and that the
// build that succeeds removes it.
func TestStaleFailureMarkerIgnored(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	tpl := countingTemplate("stale-marker", &calls)
	marker := imagePath(t, dir, tpl.Key) + ".failed"
	if err := os.WriteFile(marker, []byte("pgtest: migrate: an old failure"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	img, _ := buildImage(t, tpl)
	checkImage(t, img)
	if n := calls.Load(); n != 1 {
		t.Fatalf("Migrate ran %d times, want 1", n)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the failure marker survives a successful build: %v", err)
	}
}

// TestFutureFailureMarkerIgnored checks that a failure marker that was
// there before the wait is stale, whatever its modification time says,
// for example with a clock that runs ahead. The build runs.
func TestFutureFailureMarkerIgnored(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	tpl := countingTemplate("future-marker", &calls)
	marker := imagePath(t, dir, tpl.Key) + ".failed"
	if err := os.WriteFile(marker, []byte("pgtest: migrate: a failure from a clock ahead"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(marker, future, future); err != nil {
		t.Fatal(err)
	}
	img, _ := buildImage(t, tpl)
	checkImage(t, img)
	if n := calls.Load(); n != 1 {
		t.Fatalf("Migrate ran %d times, want 1", n)
	}
}

// TestFailureMarkerWhileWaiting checks that a failure marker that appears,
// or is replaced, while templateImage waits on the lock counts as another
// process's failed build. That holds whatever its modification time says,
// for example with a clock that runs behind or a coarse file system
// timestamp. templateImage reports the failure and does not run Migrate.
// The test holds the lock and writes the marker as a builder does. In
// "replaced", the new marker has the old one's bytes and modification
// time, so only the file (os.SameFile) tells them apart.
func TestFailureMarkerWhileWaiting(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "appears"
		if existing {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			dir := useCacheDir(t)
			var calls atomic.Int32
			tpl := countingTemplate("marker-while-waiting", &calls)
			path := imagePath(t, dir, tpl.Key)
			marker := path + ".failed"
			old := time.Now().Add(-time.Hour)
			const boom = "pgtest: migrate: boom: the other process failed"
			var before os.FileInfo
			if existing {
				if err := os.WriteFile(marker, []byte(boom), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(marker, old, old); err != nil {
					t.Fatal(err)
				}
				var err error
				if before, err = os.Stat(marker); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := lockFile(path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			unlocked := false
			defer func() {
				if !unlocked {
					unlockFile(lock)
				}
			}()
			// Learn when templateImage blocks on the lock.
			waiting := make(chan struct{}, 1)
			oldFlock := flock
			flock = func(fd, how int) error {
				if how == syscall.LOCK_EX {
					select {
					case waiting <- struct{}{}:
					default:
					}
				}
				return oldFlock(fd, how)
			}
			t.Cleanup(func() { flock = oldFlock })

			type result struct {
				img []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				img, err := templateImage(context.Background(), tpl, func(string, ...any) {})
				done <- result{img, err}
			}()
			select {
			case <-waiting:
			case r := <-done:
				t.Fatalf("templateImage returned (%d bytes, %v) without waiting on the lock", len(r.img), r.err)
			}
			if err := writeFileAtomic(marker, []byte(boom)); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
			if existing {
				after, err := os.Stat(marker)
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
					t.Fatalf("the new marker differs from the old in more than its file: same file %v, mtimes %v and %v, sizes %d and %d",
						os.SameFile(before, after), before.ModTime(), after.ModTime(), before.Size(), after.Size())
				}
			}
			unlocked = true
			if err := unlockFile(lock); err != nil {
				t.Fatal(err)
			}
			r := <-done
			want := "pgtest: template build failed in another process: " + boom
			if r.err == nil || r.err.Error() != want {
				t.Errorf("templateImage: %d bytes, error %v; want error %q", len(r.img), r.err, want)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("Migrate ran %d times, want 0", n)
			}
		})
	}
}

// TestTwoProcessesFailingBuild runs two copies of the test binary that
// build the same failing template at once (TestHelperFailingBuild). The
// first runs Migrate, which fails. The other waits on the lock, finds the
// failure marker and reports that error without running Migrate. This
// process starts no server.
func TestTwoProcessesFailingBuild(t *testing.T) {
	dir := useCacheDir(t)
	counter := filepath.Join(t.TempDir(), "migrations")
	outs := runHelpers(t, "^TestHelperFailingBuild$", "PGTEST_HELPER_COUNTER="+counter)
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 {
		t.Fatalf("Migrate ran %d times (%q), want once", len(lines), lines)
	}
	const boom = "pgtest: migrate: boom: migration 3 failed"
	waiters := 0
	for i, out := range outs {
		if !strings.Contains(out, "templateImage error: ") || !strings.Contains(out, boom) {
			t.Errorf("child %d does not report %q", i, boom)
		}
		if strings.Contains(out, "templateImage error: pgtest: template build failed in another process: "+boom) {
			waiters++
		}
	}
	if waiters != 1 {
		t.Fatalf("%d children report the other's failure, want 1", waiters)
	}
	if _, err := os.Stat(imagePath(t, dir, "failing-race") + ".failed"); err != nil {
		t.Fatalf("failure marker: %v", err)
	}
}

// TestHelperFailingBuild is the child of TestTwoProcessesFailingBuild. Its
// Migrate appends a line to $PGTEST_HELPER_COUNTER, sleeps 300 ms and
// fails. It prints templateImage's error on stdout.
func TestHelperFailingBuild(t *testing.T) {
	if os.Getenv("PGTEST_HELPER") != "1" {
		t.Skip("run by TestTwoProcessesFailingBuild")
	}
	counter := os.Getenv("PGTEST_HELPER_COUNTER")
	tpl := Template{Key: "failing-race", Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		f, err := os.OpenFile(counter, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "migrated by pid %d\n", os.Getpid())
		if err := f.Close(); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		return errors.New("boom: migration 3 failed")
	}}
	_, err := templateImage(context.Background(), tpl, func(string, ...any) {})
	fmt.Printf("templateImage error: %v\n", err)
}

// TestCachePrunesOldImages checks that writing an image removes the images
// not used for 30 days, with their lock and failure files. It also removes
// lock and failure files that old with no image, and temporary files that
// old, which a builder killed before its rename left. It touches only
// pgtest's names, <16 hex>-<16 hex>.img and the files that go with them.
func TestCachePrunesOldImages(t *testing.T) {
	dir := useCacheDir(t)
	old := time.Now().Add(-31 * 24 * time.Hour)
	recent := time.Now().Add(-29 * 24 * time.Hour)
	const (
		oldImg     = "0123456789abcdef-00000000000000a1.img"
		recentImg  = "0123456789abcdef-00000000000000a2.img"
		orphan     = "0123456789abcdef-00000000000000a3.img"
		orphan2    = "0123456789abcdef-00000000000000a4.img"
		temps      = "0123456789abcdef-00000000000000a5.img"
		upper      = "0123456789ABCDEF-00000000000000A6.img"
		shortName  = "0123456789abcdef-a7.img"
		notPgtests = "backup.img"
	)
	files := map[string]time.Time{
		oldImg:                    old,
		oldImg + ".lock":          recent, // goes with its image
		oldImg + ".failed":        recent,
		recentImg:                 recent,
		recentImg + ".lock":       old, // stays with its image
		orphan + ".lock":          old,
		orphan + ".failed":        old,
		orphan2 + ".lock":         recent,
		orphan2 + ".failed":       recent,
		temps + ".failed.tmp-123": old, // orphaned temporary files
		temps + ".tmp-124":        old,
		temps + ".failed.tmp-125": recent, // may be in use
		temps + ".tmp-126":        recent,
		upper:                     old, // not pgtest's names
		upper + ".lock":           old,
		shortName:                 old,
		shortName + ".failed":     old,
		notPgtests:                old,
		notPgtests + ".lock":      old,
		notPgtests + ".failed":    old,
		notPgtests + ".tmp-127":   old,
		"unrelated.txt":           old,
		"unrelated.txt.failed":    old,
	}
	for name, mtime := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	tpl := countingTemplate("prune", &calls)
	buildImage(t, tpl)
	path := imagePath(t, dir, tpl.Key)
	want := []string{
		filepath.Base(path), filepath.Base(path) + ".lock",
		recentImg, recentImg + ".lock",
		orphan2 + ".lock", orphan2 + ".failed",
		temps + ".failed.tmp-125", temps + ".tmp-126",
		upper, upper + ".lock",
		shortName, shortName + ".failed",
		notPgtests, notPgtests + ".lock", notPgtests + ".failed", notPgtests + ".tmp-127",
		"unrelated.txt", "unrelated.txt.failed",
	}
	if got := cacheFiles(t, dir); !equalSets(got, want) {
		t.Fatalf("cache dir holds %q, want %q", got, want)
	}
}

// TestCacheHitTouchesImage checks that reading a cached image sets its
// modification time, so that an image in use is never pruned.
func TestCacheHitTouchesImage(t *testing.T) {
	dir := useCacheDir(t)
	var calls atomic.Int32
	tpl := countingTemplate("touch", &calls)
	buildImage(t, tpl)
	path := imagePath(t, dir, tpl.Key)
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	buildImage(t, tpl)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(info.ModTime()); age > time.Minute {
		t.Fatalf("the image's modification time is %v old after a cache hit", age)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("Migrate ran %d times, want 1", n)
	}
}

// equalSets reports whether a and b hold the same strings, in any order.
func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s]--; seen[s] < 0 {
			return false
		}
	}
	return true
}
