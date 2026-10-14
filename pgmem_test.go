package pgmem_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"

	"github.com/feverenergy/pgmem"
)

// scaled multiplies a test's time bound by PGMEM_TIME_SCALE, which
// defaults to 1. make test-linux-amd64 runs under CPU emulation and sets
// it to 10.
func scaled(d time.Duration) time.Duration {
	if n, err := strconv.Atoi(os.Getenv("PGMEM_TIME_SCALE")); err == nil && n > 1 {
		return d * time.Duration(n)
	}
	return d
}

// notBuilt reports whether a test should skip on err. It should when
// PGMEM_BUILD_DIR names an incomplete local build, unless
// PGMEM_REQUIRE_BUILD is set. The default engine is always there.
func notBuilt(err error) bool {
	return errors.Is(err, fs.ErrNotExist) && os.Getenv("PGMEM_BUILD_DIR") != "" && os.Getenv("PGMEM_REQUIRE_BUILD") == ""
}

// startFails returns the error of a Start that a test expects to fail. If
// Start succeeded after all, it closes the server, so a failing assertion
// does not leave a server running, and returns nil.
func startFails(s *pgmem.Server, err error) error {
	if s != nil {
		s.Close()
	}
	return err
}

// start starts a server for one test and closes it at cleanup, checking
// that every guest process ended.
func start(t *testing.T, opts pgmem.Options) *pgmem.Server {
	t.Helper()
	return startWith(t, opts, pgmem.Start)
}

// startWith is start with another start function, such as
// pgmem.StartPreempt.
func startWith(t *testing.T, opts pgmem.Options, startFn func(context.Context, pgmem.Options) (*pgmem.Server, error)) *pgmem.Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaled(60*time.Second))
	defer cancel()
	s, err := startFn(ctx, opts)
	if notBuilt(err) {
		t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if n := pgmem.Kernel(s).ProcCount(); n != 0 {
			t.Errorf("%d processes left after Close", n)
		}
		if t.Failed() {
			t.Logf("server log:\n%s", s.Log())
		}
	})
	return s
}

// connConfig is pgx's config for s. Every dial, including CancelRequests,
// goes through s.Dial, and a cancelled context sends a CancelRequest.
func connConfig(t testing.TB, s *pgmem.Server) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig("host=/tmp port=5432 user=postgres dbname=postgres sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DialFunc = s.Dial
	cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: scaled(5 * time.Second)}
	}
	return cfg
}

func connect(t *testing.T, s *pgmem.Server) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaled(30*time.Second))
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, connConfig(t, s))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func TestStartSelect1(t *testing.T) {
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	var one int
	if err := c.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: %d %v", one, err)
	}
	if !strings.Contains(s.Log(), "database system is ready to accept connections") {
		t.Fatalf("log lacks the ready line:\n%s", s.Log())
	}
}

// TestStartFromDefaultEngine checks that with PGMEM_BUILD_DIR unset, Start
// with the zero Engine runs Postgres17's build and image and never loads
// a build directory. Empty counts as unset. With it set, Start loads that
// directory.
func TestStartFromDefaultEngine(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "")
	before := pgmem.BuildDirLoads()
	s := start(t, pgmem.Options{})
	var one int
	if err := connect(t, s).QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: %d %v", one, err)
	}
	if n := pgmem.BuildDirLoads() - before; n != 0 {
		t.Fatalf("Start loaded a build directory %d times", n)
	}

	// The counter does count. An empty build directory is loaded, and it
	// fails as a missing build.
	t.Setenv("PGMEM_BUILD_DIR", t.TempDir())
	err := startFails(pgmem.Start(context.Background(), pgmem.Options{}))
	if !errors.Is(err, fs.ErrNotExist) || pgmem.BuildDirLoads()-before != 1 {
		t.Fatalf("Start from an empty build directory: %v, %d loads", err, pgmem.BuildDirLoads()-before)
	}
}

func TestSettingsApplied(t *testing.T) {
	s := start(t, pgmem.Options{Settings: map[string]string{"work_mem": "7MB"}})
	var v string
	if err := connect(t, s).QueryRow(context.Background(), "SHOW work_mem").Scan(&v); err != nil || v != "7MB" {
		t.Fatalf("work_mem %q %v", v, err)
	}
}

func TestStartFailureQuotesLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), scaled(60*time.Second))
	defer cancel()
	before := runtime.NumGoroutine()
	// A stalled Log writer must not hold up a failing Start either.
	block := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	defer unblock()
	stalled := writerFunc(func(p []byte) (int, error) { <-block; return len(p), nil })
	t0 := time.Now()
	err := startFails(pgmem.Start(ctx, pgmem.Options{Settings: map[string]string{"no_such_setting_xyz": "1"}, Log: stalled}))
	d := time.Since(t0)
	if notBuilt(err) {
		t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err == nil || !strings.Contains(err.Error(), "no_such_setting_xyz") {
		t.Fatalf("error does not quote the log: %v", err)
	}
	if d > scaled(10*time.Second) {
		t.Fatalf("failing Start took %v", d)
	}
	unblock() // release the copier's abandoned Write
	checkGoroutines(t, before)
}

// checkGoroutines fails the test if more than before goroutines are still
// running after a short settle. A failed Start must leave none behind.
func checkGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(scaled(2 * time.Second))
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines, %d before Start:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseWithOpenConnections(t *testing.T) {
	s := start(t, pgmem.Options{})
	busy := connect(t, s)
	connect(t, s) // idle
	done := make(chan error, 1)
	go func() {
		_, err := busy.Exec(context.Background(), "SELECT pg_sleep(60)")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	t0 := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > scaled(10*time.Second) {
		t.Fatalf("Close took %v", d)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("busy query succeeded after Close")
		}
	case <-time.After(scaled(5 * time.Second)):
		t.Fatal("busy query still running after Close")
	}
	if _, err := s.Dial(context.Background(), "unix", ""); err == nil {
		t.Fatal("Dial after Close succeeded")
	}
}

func TestLogWriterNeverBlocks(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	s := start(t, pgmem.Options{Log: writerFunc(func(p []byte) (int, error) { <-block; return len(p), nil })})
	// A stalled Log writer must not stall the server.
	for range 20 {
		c := connect(t, s)
		c.Close(context.Background())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestReservedSettings checks that Start refuses settings that would move
// the data directory, its configuration files or the socket, before it
// loads anything. The build directory here is empty.
func TestReservedSettings(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", t.TempDir())
	for _, name := range []string{"port", "unix_socket_directories", "listen_addresses", "data_directory", "hba_file", "config_file", "PORT", "Unix-Socket-Directories"} {
		err := startFails(pgmem.Start(context.Background(), pgmem.Options{Settings: map[string]string{name: "x"}}))
		if err == nil || !strings.Contains(err.Error(), "reserved") || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Settings %q: got %v, want a reserved-setting error", name, err)
		}
	}
}

// TestRelativeBuildDir checks that a relative PGMEM_BUILD_DIR that does
// not exist fails as a missing build. The error names the absolute
// directory it resolved to, because go test runs each package in its own
// directory.
func TestRelativeBuildDir(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", filepath.Join("no", "such", "build"))
	abs, err := filepath.Abs(filepath.Join("no", "such", "build"))
	if err != nil {
		t.Fatal(err)
	}
	err = startFails(pgmem.Start(context.Background(), pgmem.Options{}))
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "(relative: "+abs+")") {
		t.Fatalf("Start from a relative, missing build directory: %v", err)
	}
}

// TestStartDoneContext checks that Start fails at once on a done context,
// before it loads anything. The build directory here is empty.
func TestStartDoneContext(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := startFails(pgmem.Start(ctx, pgmem.Options{})); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start with a cancelled context: %v, want context.Canceled", err)
	}
}

// TestUnusableCacheDir checks that when the compile cache directory cannot
// be created or written, Start compiles without it and logs one line.
func TestUnusableCacheDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readOnly, 0o700) })
	for _, tc := range []struct{ name, dir string }{
		{"under-a-file", filepath.Join(file, "cache")},
		{"read-only", readOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "read-only" && os.Geteuid() == 0 {
				t.Skip("root can write a read-only directory")
			}
			defer pgmem.SetCacheDir(tc.dir)()
			s := start(t, pgmem.Options{})
			c := connect(t, s)
			var one int
			if err := c.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("SELECT 1: %d %v", one, err)
			}
			if n := strings.Count(s.Log(), "pgmem: compiling without a cache"); n != 1 {
				t.Fatalf("log has %d cache lines, want 1:\n%s", n, s.Log())
			}
		})
	}
}

// TestCloseEscalation checks Close's escalation. When fast shutdown does
// not finish in time, Close sends SIGQUIT and logs it. When that does not
// finish either, the kernel kills what is left, and Close logs that. When
// the kernel's close times out too, Close reports it and the close
// finishes in the background.
func TestCloseEscalation(t *testing.T) {
	const quitLine = "; sent SIGQUIT" // its cause varies; see TestShutdownEscalates
	const killLine = "pgmem: immediate shutdown timed out; killing remaining processes"
	t.Run("quit", func(t *testing.T) {
		defer pgmem.SetCloseWaits(time.Nanosecond, scaled(5*time.Second), scaled(5*time.Second), nil)()
		s := start(t, pgmem.Options{})
		connect(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if log := s.Log(); !strings.Contains(log, quitLine) || strings.Contains(log, killLine) {
			t.Fatalf("log lacks %q or has %q:\n%s", quitLine, killLine, log)
		}
	})
	t.Run("kill", func(t *testing.T) {
		closed := make(chan error, 1)
		defer pgmem.SetCloseWaits(time.Nanosecond, time.Nanosecond, time.Nanosecond, func(err error) { closed <- err })()
		ctx, cancel := context.WithTimeout(context.Background(), scaled(60*time.Second))
		defer cancel()
		s, err := pgmem.Start(ctx, pgmem.Options{})
		if notBuilt(err) {
			t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
		}
		if err != nil {
			t.Fatal(err)
		}
		connect(t, s)
		if err := s.Close(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close: %v, want the kernel close to time out", err)
		}
		if log := s.Log(); !strings.Contains(log, quitLine) || !strings.Contains(log, killLine) {
			t.Fatalf("log lacks %q or %q:\n%s", quitLine, killLine, log)
		}
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("background kernel close: %v", err)
			}
		case <-time.After(scaled(10 * time.Second)):
			t.Fatal("the kernel close left in the background never finished")
		}
		if n := pgmem.Kernel(s).ProcCount(); n != 0 {
			t.Fatalf("%d processes left after the background close", n)
		}
	})
}
