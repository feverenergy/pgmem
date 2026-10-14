package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// lineWriter collects what run writes and hands each complete line to
// lines, if set.
type lineWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	part  []byte
	lines chan string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			break
		}
		line := string(w.part[:i])
		w.part = w.part[i+1:]
		if w.lines != nil {
			select {
			case w.lines <- line:
			default:
			}
		}
	}
	return len(p), nil
}

func (w *lineWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// shortTemp returns a new directory under /tmp, short enough for a socket
// path, and removes it at cleanup. macOS allows 103 bytes.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pgm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// served is a pgmem-serve running in the test process.
type served struct {
	sock   string // the socket path from the ready line
	before int    // goroutines before run
	stderr *lineWriter
	cancel context.CancelFunc
	done   chan error // run's result
}

// notBuilt reports whether a test should skip on err. It should when
// PGMEM_BUILD_DIR names an incomplete local build, unless
// PGMEM_REQUIRE_BUILD is set. The default engine is always there.
func notBuilt(err error) bool {
	return errors.Is(err, fs.ErrNotExist) && os.Getenv("PGMEM_BUILD_DIR") != "" && os.Getenv("PGMEM_REQUIRE_BUILD") == ""
}

// serve runs pgmem-serve with args until the test ends or calls stop, and
// waits for its ready line. It skips if PGMEM_BUILD_DIR names an
// incomplete build. See notBuilt.
func serve(t *testing.T, args ...string) *served {
	t.Helper()
	clearPGEnv(t)
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{
		before: before,
		stderr: &lineWriter{},
		cancel: cancel,
		done:   make(chan error, 1),
	}
	stdout := &lineWriter{lines: make(chan string, 16)}
	go func() { s.done <- run(ctx, args, stdout, s.stderr) }()
	t.Cleanup(func() {
		if err := s.stop(); err != nil {
			t.Errorf("run: %v", err)
		}
		checkGoroutines(t, before)
		if strings.Contains(s.stderr.String(), "still running") {
			t.Error("sessions outlived the proxy's close")
		}
		if t.Failed() {
			t.Logf("stderr:\n%s", s.stderr)
		}
	})
	timeout := time.After(60 * time.Second)
	for {
		select {
		case line := <-stdout.lines:
			if p, ok := strings.CutPrefix(line, "ready "); ok {
				s.sock = p
				return s
			}
			t.Fatalf("unexpected stdout line %q", line)
		case err := <-s.done:
			s.done <- err // for stop
			if notBuilt(err) {
				t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
			}
			t.Fatalf("run ended before ready: %v", err)
		case <-timeout:
			t.Fatal("no ready line within 60 s")
		}
	}
}

// checkGoroutines fails the test if more than before goroutines are still
// running after a short settle. run must leave none behind.
func checkGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Errorf("%d goroutines, %d before run:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stop cancels run, as SIGINT would, and returns its result.
func (s *served) stop() error {
	s.cancel()
	select {
	case err := <-s.done:
		s.done <- err
		return err
	case <-time.After(30 * time.Second):
		return errors.New("run did not return within 30 s of cancel")
	}
}

// clearPGEnv unsets libpq's connection variables, so that nothing in the
// environment can steer pgx to another server.
func clearPGEnv(t *testing.T) {
	for _, v := range []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGUSER",
		"PGPASSWORD", "PGPASSFILE", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGOPTIONS",
		"PGAPPNAME", "PGCONNECT_TIMEOUT", "PGTARGETSESSIONATTRS"} {
		t.Setenv(v, "")
	}
}

// connect opens a pgx connection to host and port. Over TCP, pgx first
// sends an SSLRequest because of sslmode=prefer. The proxy must pass it
// through with its one-byte refusal.
func connect(t *testing.T, host string, port string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s user=postgres dbname=postgres sslmode=prefer", host, port))
	if err != nil {
		t.Fatal(err)
	}
	// A cancelled context sends a CancelRequest, through the proxy.
	cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: 10 * time.Second}
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// connectSock connects through the Unix socket path sock.
func connectSock(t *testing.T, sock string) *pgx.Conn {
	t.Helper()
	dir, name := filepath.Split(sock)
	port, ok := strings.CutPrefix(name, ".s.PGSQL.")
	if !ok {
		t.Fatalf("socket %q is not named .s.PGSQL.<port>", sock)
	}
	return connect(t, filepath.Clean(dir), port)
}

// TestServeRefusesBadFlags checks the flag validation alone, so a
// regression cannot make the test listen anywhere.
func TestServeRefusesBadFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-tcp", "127.0.0.1:65536"}, "bad port"},
		{[]string{"-tcp", "0.0.0.0:54329"}, "loopback"},
		{[]string{"-tcp", ":54329"}, "loopback"},
		{[]string{"-tcp", "[::]:54329"}, "loopback"},
		{[]string{"-tcp", "192.168.1.1:54329"}, "loopback"},
		{[]string{"-tcp", "127.0.0.1:postgresql"}, "number"},
		{[]string{"-port", "-1"}, "bad port"},
		{[]string{"-in", "/"}, "too broad"},
		{[]string{"-out", "/tmp"}, "too broad"},
		{[]string{"-out", "/pgdata/x"}, "/pgdata"},
		{[]string{"-out", "/pgmem"}, "/pgmem"},
		{[]string{"-in", "/dev"}, "/dev"},
		{[]string{"-max-rss-mb", "0"}, "positive"},
	} {
		_, err := parseFlags(tc.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got error %v, want one naming %q", tc.args, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"-tcp", "127.0.0.1:0"},
		{"-tcp", "[::1]:0"},
		{"-tcp", "localhost:54329"},
		{"-port", "54329", "-out", "/tmp/pgm-results"},
	} {
		if _, err := parseFlags(args, io.Discard); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestServeProxiesQueries(t *testing.T) {
	dir := shortTemp(t)
	// -out makes the proxy parse the message framing, which includes
	// passing through the one-byte answer to pgx's SSLRequest over TCP.
	s := serve(t, "-socket", dir, "-port", "54329", "-tcp", "127.0.0.1:0", "-out", t.TempDir())
	if want := filepath.Join(dir, ".s.PGSQL.54329"); s.sock != want {
		t.Fatalf("ready %q, want %q", s.sock, want)
	}
	c := connect(t, dir, "54329")
	var one int
	if err := c.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: %d, %v", one, err)
	}

	// Five sessions at once, each with its own backend.
	var wg sync.WaitGroup
	pids := make([]int32, 5)
	for i := range pids {
		c := connect(t, dir, "54329")
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.QueryRow(ctx, "SELECT pg_backend_pid() FROM pg_sleep(0.2)").Scan(&pids[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	seen := map[int32]bool{}
	for _, p := range pids {
		if p == 0 || seen[p] {
			t.Errorf("backend pids %v: want five distinct", pids)
			break
		}
		seen[p] = true
	}

	// A context cancel sends a CancelRequest through the proxy, and the
	// session survives it.
	cctx, ccancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t0 := time.Now()
	_, err := c.Exec(cctx, "SELECT pg_sleep(30)")
	ccancel()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("cancelled pg_sleep: %v, want SQLSTATE 57014", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("cancel took %v", d)
	}
	if err := c.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Errorf("SELECT 1 after the cancel: %d, %v", one, err)
	}

	// TCP, on the address it reports.
	m := regexp.MustCompile(`listening on tcp 127\.0\.0\.1:(\d+)`).FindStringSubmatch(s.stderr.String())
	if m == nil {
		t.Fatal("no TCP address in the log")
	}
	tc := connect(t, "127.0.0.1", m[1])
	if err := tc.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 over TCP: %d, %v", one, err)
	}

	// Stopping with sessions open ends every proxy goroutine and removes
	// the socket.
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	checkGoroutines(t, s.before)
	if strings.Contains(s.stderr.String(), "still running") {
		t.Error("sessions outlived the proxy's close")
	}
	if _, err := os.Stat(s.sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Error("a session survived the stop")
	}
}

func TestServeInOutSync(t *testing.T) {
	in := t.TempDir()
	out := filepath.Join(t.TempDir(), "results")
	if err := os.MkdirAll(filepath.Join(in, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "sub", "in.txt"), []byte("hello from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := serve(t, "-socket", shortTemp(t), "-in", in, "-out", out)
	c := connectSock(t, s.sock)
	ctx := context.Background()

	// With -in, the host tree is in memfs at the same path.
	var got string
	if err := c.QueryRow(ctx, "SELECT pg_read_file($1)", filepath.Join(in, "sub", "in.txt")).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "hello from the host\n" {
		t.Errorf("pg_read_file: %q", got)
	}

	// With -out, memfs to host, the file is on the host once the query
	// returns.
	if _, err := c.Exec(ctx, fmt.Sprintf("COPY (SELECT 42) TO '%s/x'", out)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "x")); err != nil || string(b) != "42\n" {
		t.Errorf("host %s/x: %q, %v", out, b, err)
	}

	// With -out, host to memfs, a host file is in memfs for the query that
	// names it.
	if err := os.WriteFile(filepath.Join(out, "y"), []byte("7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE TEMP TABLE t (i int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("COPY t FROM '%s/y'", out)); err != nil {
		t.Fatal(err)
	}
	var i int
	if err := c.QueryRow(ctx, "SELECT i FROM t").Scan(&i); err != nil || i != 7 {
		t.Errorf("COPY FROM a host file: %d, %v", i, err)
	}

	// The host side never follows a symlink inside DIR. A server write to
	// <out>/link leaves the link's target alone and is logged.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(out, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("COPY (SELECT 1) TO '%s/link'", out)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "untouched\n" {
		t.Errorf("symlink target: %q, %v", b, err)
	}
	if !strings.Contains(s.stderr.String(), "sync out: "+filepath.Join(out, "link")) {
		t.Errorf("the refused write to %s/link was not logged", out)
	}
}

func TestServeDefaultSocketRemoved(t *testing.T) {
	statsFile := filepath.Join(t.TempDir(), "stats")
	s := serve(t, "-stats", statsFile, "-lib-suffix", ".dylib")
	if !strings.HasPrefix(s.sock, "/tmp/") || filepath.Base(s.sock) != ".s.PGSQL.54329" {
		t.Fatalf("default socket %q", s.sock)
	}
	c := connectSock(t, s.sock)
	// The placeholder under the extra suffix loads the linked library.
	if _, err := c.Exec(context.Background(), "LOAD 'autoinc.dylib'"); err != nil {
		t.Errorf("LOAD 'autoinc.dylib': %v", err)
	}
	c.Close(context.Background())
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(s.sock)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket directory left behind: %v", err)
	}
	b, err := os.ReadFile(statsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/sched/pauses/total/gc:seconds", "peak RSS"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("stats lack %q:\n%s", want, b)
		}
	}
}

// TestServeWatchdog checks that over -max-rss-mb, run stops the server and
// fails. The watchdog samples from before Start, so a 1 MiB limit trips at
// once.
func TestServeWatchdog(t *testing.T) {
	clearPGEnv(t)
	dir := shortTemp(t)
	var stdout, stderr lineWriter
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := run(ctx, []string{"-socket", dir, "-max-rss-mb", "1"}, &stdout, &stderr)
	if notBuilt(err) {
		t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err == nil || !strings.Contains(err.Error(), "RSS") {
		t.Errorf("run: %v, want an RSS error\nstderr:\n%s", err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Error("run returned only at the test's deadline")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left in the socket directory: %v", ents)
	}
}

// fakeSampler replaces the watchdog's RSS sampler for one test. It reports
// 10 MiB until armed, then calls armedSample.
func fakeSampler(t *testing.T, armedSample func() (int64, error)) *atomic.Bool {
	var armed atomic.Bool
	old := sampleRSS
	sampleRSS = func() (int64, error) {
		if armed.Load() {
			return armedSample()
		}
		return 10 << 20, nil
	}
	t.Cleanup(func() { sampleRSS = old })
	return &armed
}

// tripWhileServing arms the sampler of a serving pgmem-serve and checks
// that run then stops the server and fails with an error containing want.
func tripWhileServing(t *testing.T, armedSample func() (int64, error), want string) {
	armed := fakeSampler(t, armedSample)
	s := serve(t, "-socket", shortTemp(t))
	c := connectSock(t, s.sock)
	armed.Store(true)
	select {
	case err := <-s.done:
		s.done <- nil // the trip is expected, so stop at cleanup reports nil
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("run: %v, want an error naming %q", err, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog did not stop the server")
	}
	if _, err := os.Stat(s.sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Error("a session survived the trip")
	}
}

func TestServeWatchdogTripsWhileServing(t *testing.T) {
	tripWhileServing(t, func() (int64, error) { return 8 << 40, nil }, "passed -max-rss-mb 4096")
}

// TestServeWatchdogTripsWhenBlind checks that four failed samples in a
// row, about a second, trip the watchdog as surely as a high RSS.
func TestServeWatchdogTripsWhenBlind(t *testing.T) {
	tripWhileServing(t, func() (int64, error) { return 0, errors.New("no RSS") }, "unreadable for 4 samples")
}

func TestServeSecondSignalExits(t *testing.T) {
	sig := make(chan os.Signal, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go handleSignals(sig, cancel, io.Discard, func(code int) { exited <- code })
	sig <- syscall.SIGTERM
	<-ctx.Done()
	select {
	case code := <-exited:
		t.Fatalf("exit(%d) after one signal", code)
	case <-time.After(50 * time.Millisecond):
	}
	sig <- os.Interrupt
	select {
	case code := <-exited:
		if code != 2 {
			t.Errorf("exit(%d), want 2", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit after a second signal")
	}
}

// -h warns that every client is superuser, and what -tcp and -out expose.
func TestUsageWarns(t *testing.T) {
	var out bytes.Buffer
	if _, err := parseFlags([]string{"-h"}, &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h): %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"usage: pgmem-serve", "trust authentication", "superuser", "With -tcp, that is every local user", "as the user running pgmem-serve"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, out.String())
		}
	}
}
