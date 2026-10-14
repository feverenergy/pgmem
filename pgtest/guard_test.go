package pgtest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setParallel sets the -test.parallel flag to n for one test.
func setParallel(t *testing.T, n int) {
	t.Helper()
	f := flag.Lookup("test.parallel")
	if f == nil {
		t.Fatal("no test.parallel flag")
	}
	old := f.Value.String()
	if err := f.Value.Set(strconv.Itoa(n)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Value.Set(old) })
}

func TestMaxConnectionsFormula(t *testing.T) {
	for _, tc := range []struct{ parallel, want int }{
		{1, 500}, {9, 500}, {24, 500}, {25, 520}, {64, 1300}, {130, 2620}, {131, 2620}, {1000, 2620},
	} {
		setParallel(t, tc.parallel)
		if got := maxConnections(); got != tc.want {
			t.Errorf("maxConnections() at -test.parallel=%d = %d, want %d", tc.parallel, got, tc.want)
		}
	}
}

// TestPoolConfigClosesIdleConnections checks that a pool closes a
// connection idle for a second, so a parent test's pool does not keep
// backends while its parallel subtests wait for their turn.
func TestPoolConfigClosesIdleConnections(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", "off") // starts no server; set for the rule
	cfg := poolConfig(nil, "db_1", 20)
	if cfg.MaxConnIdleTime != time.Second || cfg.HealthCheckPeriod != time.Second {
		t.Fatalf("MaxConnIdleTime %v, HealthCheckPeriod %v; want 1s and 1s", cfg.MaxConnIdleTime, cfg.HealthCheckPeriod)
	}
}

// showMaxConnections returns the server's max_connections, through pool.
func showMaxConnections(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), "SHOW max_connections").Scan(&s); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMaxConnectionsCoversParallelPools checks that at -test.parallel=16
// the server has room for 16 pools of 20 connections, and that 64
// connections across 4 pools open at once. The SHOW check proves the
// headroom. The burst only shows that the connections open.
func TestMaxConnectionsCoversParallelPools(t *testing.T) {
	setParallel(t, 16)
	setup(t)
	pools := make([]*pgxpool.Pool, 4)
	for i := range pools {
		pools[i] = Pool(t, seedTemplate)
	}
	if n := showMaxConnections(t, pools[0]); n < 340 {
		t.Fatalf("max_connections %d, want at least 340 (16 pools of 20, plus 20)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for _, pool := range pools {
		for range 16 {
			wg.Go(func() {
				conn, err := pool.Acquire(ctx)
				if err != nil {
					errs <- err
					return
				}
				defer conn.Release()
				// Hold every connection until all 64 are open.
				if _, err := conn.Exec(ctx, "SELECT pg_sleep(0.2)"); err != nil {
					errs <- err
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var open int
	if err := pools[0].QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname LIKE 'db\\_%'").Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open < 64 {
		t.Errorf("%d sessions on the test databases after the burst, want at least 64", open)
	}
}

// TestMaxConnectionsAtCap checks that the server starts with
// max_connections at its cap, 2620, which any -test.parallel past 130
// gets. It opens no more connections than it needs.
func TestMaxConnectionsAtCap(t *testing.T) {
	setParallel(t, 1000)
	setup(t)
	pool := Pool(t, seedTemplate, WithMaxConns(2))
	if n := showMaxConnections(t, pool); n != 2620 {
		t.Fatalf("max_connections %d, want 2620 (maxConnections() is %d)", n, maxConnections())
	}
}

// TestParkedParentsHeadroom runs TestHelperParkedParents in a child
// process at -test.parallel=2. The child runs 30 parallel parent tests,
// each with a pool that serves 4 parallel subtests. A parent's function
// returns and releases its slot before its subtests run, so up to 30 pools
// exist at once, not 2. The child checks that no connection fails and
// that idle connections do not pile up. This process starts no server.
func TestParkedParentsHeadroom(t *testing.T) {
	useCacheDir(t)
	resetForTest(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperParkedParents$", "-test.parallel=2", "-test.v", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), "PGTEST_HELPER=parked")
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if !strings.Contains(string(out), "--- PASS: TestHelperParkedParents") {
		t.Fatal("the child did not run TestHelperParkedParents")
	}
}

// TestHelperParkedParents is the child of TestParkedParentsHeadroom. A
// sampler counts the server's client backends every 20 ms. Each parent
// opens 2 connections at once before its subtests, and each subtest holds
// 2 for 150 ms. The parents run first, because go test hands out the slots
// in order. So from lateAfter after the last parent's setup on, a parent
// waiting for its subtests must have closed its idle connections. The
// backends then belong to the running subtests and to at most a few pools
// in use.
func TestHelperParkedParents(t *testing.T) {
	if os.Getenv("PGTEST_HELPER") != "parked" {
		t.Skip("run by TestParkedParentsHeadroom")
	}
	resetForTest(t)
	t.Cleanup(func() { resetForTest(t) })
	first := Pool(t, seedTemplate, WithMaxConns(1)) // starts the server
	maxConns := showMaxConnections(t, first)
	admin := serverFor(seedTemplate).admin

	type sample struct {
		at time.Time
		n  int
	}
	var (
		mu        sync.Mutex
		samples   []sample
		lastSetup time.Time
	)
	stop := make(chan struct{})
	sampled := make(chan error, 1)
	go func() {
		ctx := context.Background()
		for {
			var n int
			if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'").Scan(&n); err != nil {
				sampled <- err
				return
			}
			mu.Lock()
			samples = append(samples, sample{time.Now(), n})
			mu.Unlock()
			select {
			case <-stop:
				sampled <- nil
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	hold := func(t *testing.T, pool *pgxpool.Pool, n int, d time.Duration) {
		ctx := context.Background()
		var conns []*pgxpool.Conn
		for range n {
			c, err := pool.Acquire(ctx)
			if err != nil {
				t.Error(err)
				break
			}
			conns = append(conns, c)
		}
		time.Sleep(d)
		for _, c := range conns {
			if _, err := c.Exec(ctx, "SELECT 1"); err != nil {
				t.Error(err)
			}
			c.Release()
		}
	}
	t.Run("parents", func(t *testing.T) {
		for i := range 30 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				pool := Pool(t, seedTemplate)
				hold(t, pool, 2, 0)
				mu.Lock()
				lastSetup = time.Now()
				mu.Unlock()
				for j := range 4 {
					t.Run(fmt.Sprint(j), func(t *testing.T) {
						t.Parallel()
						hold(t, pool, 2, 150*time.Millisecond)
					})
				}
			})
		}
	})
	close(stop)
	if err := <-sampled; err != nil {
		t.Fatal(err)
	}

	// An idle connection closes 1 s after its release, at the pool's next
	// health check (every 1 s), and its backend then takes a moment to
	// exit. On an unloaded machine that is 2 s at most. The rest is margin
	// for a loaded machine.
	const lateAfter = 4 * time.Second
	peak, late, nLate := 0, 0, 0
	for _, s := range samples {
		peak = max(peak, s.n)
		if s.at.After(lastSetup.Add(lateAfter)) {
			late = max(late, s.n)
			nLate++
		}
	}
	t.Logf("max_connections %d; %d samples: peak %d client backends; %d samples from %v after the last parent's setup: peak %d",
		maxConns, len(samples), peak, nLate, lateAfter, late)
	if maxConns < 500 {
		t.Errorf("max_connections %d, want at least 500", maxConns)
	}
	if peak >= maxConns {
		t.Errorf("peak %d client backends, at max_connections %d", peak, maxConns)
	}
	if nLate < 50 {
		t.Fatalf("only %d samples after the parents' idle connections should have closed", nLate)
	}
	if late > 24 {
		t.Errorf("peak %d client backends while parents waited for their subtests, want at most 24: idle connections are kept", late)
	}
}

// syncBuffer is a strings.Builder safe for the guard's goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// guardFakes are what injectGuard put in place of the guard's sampler,
// exit function and stderr.
type guardFakes struct {
	samples atomic.Int64 // how many samples were taken
	exited  chan int
	stderr  syncBuffer
}

// injectGuard replaces the guard's sampler with sample, its exit function
// with one that records the code, its stderr with a buffer, and its period
// with 10 ms, for one test. The guard copies them when it starts, so a
// guard of an earlier test keeps its own.
func injectGuard(t *testing.T, sample func() (int64, error)) *guardFakes {
	t.Helper()
	f := &guardFakes{exited: make(chan int, 1)}
	oldSample, oldExit, oldStderr, oldPeriod := guardSample, guardExit, guardStderr, guardPeriod
	guardSample = func() (int64, error) {
		f.samples.Add(1)
		return sample()
	}
	guardExit = func(code int) { f.exited <- code }
	guardStderr = &f.stderr
	guardPeriod = 10 * time.Millisecond
	t.Cleanup(func() {
		guardSample, guardExit, guardStderr, guardPeriod = oldSample, oldExit, oldStderr, oldPeriod
	})
	return f
}

// TestGuardTrips checks that the guard starts with the first server, and
// that past PGTEST_MAX_RSS_MB it writes the message and the server's log,
// then exits with code 3.
func TestGuardTrips(t *testing.T) {
	t.Setenv("PGTEST_MAX_RSS_MB", "100")
	var armed atomic.Bool
	f := injectGuard(t, func() (int64, error) {
		if armed.Load() {
			return 200 << 20, nil
		}
		return 10 << 20, nil
	})
	setup(t)
	pool := Pool(t, seedTemplate)
	// 30 errors log 60 lines (ERROR and STATEMENT), past the 50 written.
	for i := range 30 {
		if _, err := pool.Exec(context.Background(), fmt.Sprintf("SELECT 'line %d'::int", i)); err == nil {
			t.Fatal("a bad cast succeeded")
		}
	}
	if f.samples.Load() == 0 {
		t.Fatal("the guard has not sampled since the server started")
	}
	armed.Store(true)
	select {
	case code := <-f.exited:
		if code != 3 {
			t.Errorf("exit(%d), want exit(3)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the guard did not exit")
	}
	out := f.stderr.String()
	t.Logf("stderr:\n%s", out)
	msg := "pgtest: test process resident memory 200 MiB exceeds PGTEST_MAX_RSS_MB=100; aborting\n"
	if !strings.HasPrefix(out, msg) {
		t.Errorf("stderr does not start with %q", msg)
	}
	header := fmt.Sprintf("pgtest: last 50 lines of the server log for template %q (engine %s):\n", seedTemplate.Key, engineName(seedTemplate.Engine))
	if !strings.HasPrefix(out[len(msg):], header) {
		t.Errorf("the message is not followed by %q", header)
	}
	if n := strings.Count(out, "\n"); n != 2+maxLogLines {
		t.Errorf("stderr has %d lines, want %d (message, header, %d log lines)", n, 2+maxLogLines, maxLogLines)
	}
	if !strings.Contains(out, `STATEMENT:  SELECT 'line 29'::int`) || strings.Contains(out, `'line 0'`) {
		t.Errorf("stderr does not hold the most recent log lines")
	}
	// The guard ended at the trip.
	n := f.samples.Load()
	time.Sleep(100 * time.Millisecond)
	if m := f.samples.Load(); m != n {
		t.Errorf("the guard sampled %d more times after it tripped", m-n)
	}
}

// TestGuardBlindStopsGuarding checks that four failed samples in a row
// make one warning line and stop the guard, which never exits.
func TestGuardBlindStopsGuarding(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", t.TempDir()) // starts no server; set for the rule
	t.Setenv("PGTEST_MAX_RSS_MB", "100")
	f := injectGuard(t, func() (int64, error) { return 0, errors.New("no RSS") })
	resetForTest(t)
	t.Cleanup(func() { resetForTest(t) })
	startGuard()
	deadline := time.Now().Add(5 * time.Second)
	for f.stderr.String() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // ten more periods, so the guard must have stopped
	out := f.stderr.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "no RSS") || !strings.Contains(out, "PGTEST_MAX_RSS_MB") {
		t.Errorf("stderr %q, want one line naming the error and PGTEST_MAX_RSS_MB", out)
	}
	if n := f.samples.Load(); n != 4 {
		t.Errorf("%d samples, want 4", n)
	}
	select {
	case code := <-f.exited:
		t.Errorf("exit(%d) on a blind guard", code)
	default:
	}
}

// TestGuardDisabled checks that PGTEST_MAX_RSS_MB=0 turns the guard off,
// so it never samples.
func TestGuardDisabled(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", t.TempDir()) // starts no server; set for the rule
	t.Setenv("PGTEST_MAX_RSS_MB", "0")
	f := injectGuard(t, func() (int64, error) { return 1 << 40, nil })
	resetForTest(t)
	t.Cleanup(func() { resetForTest(t) })
	startGuard()
	time.Sleep(100 * time.Millisecond)
	if n := f.samples.Load(); n != 0 {
		t.Errorf("%d samples with the guard off", n)
	}
	if out := f.stderr.String(); out != "" {
		t.Errorf("stderr %q, want nothing", out)
	}
	select {
	case code := <-f.exited:
		t.Errorf("exit(%d) with the guard off", code)
	default:
	}
}

// TestGuardBadLimit checks that pgtest reports a PGTEST_MAX_RSS_MB that is
// not a number of MiB, and that the guard runs at the default 4096.
func TestGuardBadLimit(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", t.TempDir()) // starts no server; set for the rule
	// Past MaxInt64>>20 MiB, the limit in bytes would overflow.
	for _, bad := range []string{"lots", "-1", "4G", strconv.FormatInt(math.MaxInt64>>20+1, 10)} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("PGTEST_MAX_RSS_MB", bad)
			f := injectGuard(t, func() (int64, error) { return 5000 << 20, nil })
			resetForTest(t)
			t.Cleanup(func() { resetForTest(t) })
			startGuard()
			select {
			case code := <-f.exited:
				if code != 3 {
					t.Errorf("exit(%d), want exit(3)", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the guard did not exit at 5000 MiB")
			}
			out := f.stderr.String()
			if !strings.HasPrefix(out, fmt.Sprintf("pgtest: PGTEST_MAX_RSS_MB=%q", bad)) ||
				!strings.Contains(out, "resident memory 5000 MiB exceeds PGTEST_MAX_RSS_MB=4096") {
				t.Errorf("stderr %q, want a warning about %q, then a trip at 4096", out, bad)
			}
		})
	}
}
