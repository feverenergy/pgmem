package pgmem_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/feverenergy/pgmem"
)

// Concurrency, crash and churn tests against a real postmaster. Each test
// starts its own server and opens at most 22 connections.
//
// The CPU-bound queries use generate_series in the select list, under a
// subquery, rather than in FROM. A FROM-clause function scan materializes
// its whole result into a tuplestore. That spills to temporary files in
// the in-memory file system, hundreds of MB for 30 million rows. The
// select-list form streams the rows and only burns CPU.
const (
	cpuLoop2e9  = "SELECT count(*) FROM (SELECT generate_series(1, 2000000000)) s"
	cpuLoop30e6 = "SELECT count(*) FROM (SELECT generate_series(1, 30000000)) s"
)

const sigKILL = 9

// testCtx is the test's context. Every query in it ends after
// scaled(30 s) at the latest.
func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), scaled(30*time.Second))
	t.Cleanup(cancel)
	return ctx
}

// sqlstate returns err's SQLSTATE, or "" if it has none.
func sqlstate(err error) string {
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// async runs f in a goroutine. Its error arrives on the returned channel.
func async(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

// await returns the error from ch, failing the test if none arrives
// within d.
func await(t *testing.T, ch <-chan error, d time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		t.Fatalf("%s: no result within %v", what, d)
		return nil
	}
}

// notYet fails the test if ch already has a result.
func notYet(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s finished early: %v", what, err)
	default:
	}
}

// eventually polls cond every 10 ms until it reports true, failing the
// test if that takes longer than d or cond fails.
func eventually(t *testing.T, d time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func backendPID(t *testing.T, ctx context.Context, c *pgx.Conn) int32 {
	t.Helper()
	var pid int32
	if err := c.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitEvent returns pid's wait_event_type and wait_event as mon sees them
// in pg_stat_activity. Both are "" when it waits on nothing.
func waitEvent(ctx context.Context, mon *pgx.Conn, pid int32) (typ, event string, err error) {
	err = mon.QueryRow(ctx, "SELECT coalesce(wait_event_type, ''), coalesce(wait_event, '') FROM pg_stat_activity WHERE pid = $1", pid).Scan(&typ, &event)
	return typ, event, err
}

// awaitLockWait waits until pid waits on a lock.
func awaitLockWait(t *testing.T, ctx context.Context, mon *pgx.Conn, pid int32) {
	t.Helper()
	eventually(t, scaled(10*time.Second), fmt.Sprintf("backend %d waits on a lock", pid), func() (bool, error) {
		typ, _, err := waitEvent(ctx, mon, pid)
		return typ == "Lock", err
	})
}

func exec(t *testing.T, ctx context.Context, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestConcurrentConnections(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	const n = 20
	cfg := connConfig(t, s)
	conns := make([]*pgx.Conn, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { conns[i], errs[i] = pgx.ConnectConfig(ctx, cfg.Copy()) })
	}
	waitGroup(t, &wg, scaled(30*time.Second), "20 concurrent connects")
	t.Cleanup(func() {
		for _, c := range conns {
			if c != nil {
				c.Close(context.Background())
			}
		}
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	for i, c := range conns {
		wg.Go(func() {
			for j := range 50 {
				want := i*1000 + j
				var got int
				if err := c.QueryRow(ctx, "SELECT $1::int", want).Scan(&got); err != nil {
					errs[i] = fmt.Errorf("conn %d query %d: %w", i, j, err)
					return
				}
				if got != want {
					errs[i] = fmt.Errorf("conn %d query %d: got %d, want %d", i, j, got, want)
					return
				}
			}
		})
	}
	waitGroup(t, &wg, scaled(30*time.Second), "20x50 queries")
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	var clients int
	if err := conns[0].QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'").Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if clients < n {
		t.Fatalf("pg_stat_activity shows %d client backends with %d connections open", clients, n)
	}
}

// waitGroup waits for wg, failing the test after d.
func waitGroup(t *testing.T, wg *sync.WaitGroup, d time.Duration, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: not done within %v", what, d)
	}
}

func TestRowLockWait(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	a, b, mon := connect(t, s), connect(t, s), connect(t, s)
	exec(t, ctx, a, "CREATE TABLE t(v int); INSERT INTO t VALUES (1)")
	exec(t, ctx, a, "BEGIN")
	exec(t, ctx, a, "UPDATE t SET v = 2")

	bpid := backendPID(t, ctx, b)
	done := async(func() error { _, err := b.Exec(ctx, "UPDATE t SET v = v + 10"); return err })
	awaitLockWait(t, ctx, mon, bpid)
	time.Sleep(200 * time.Millisecond)
	notYet(t, done, "B's UPDATE while A holds the row lock")
	if typ, event, err := waitEvent(ctx, mon, bpid); err != nil || typ != "Lock" {
		t.Fatalf("after 200 ms B waits on %q/%q (%v), want a Lock", typ, event, err)
	}

	exec(t, ctx, a, "COMMIT")
	if err := await(t, done, scaled(5*time.Second), "B's UPDATE after A's COMMIT"); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := mon.QueryRow(ctx, "SELECT v FROM t").Scan(&v); err != nil || v != 12 {
		t.Fatalf("v = %d (%v), want 12", v, err)
	}
}

func TestDeadlockDetected(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	a, b, mon := connect(t, s), connect(t, s), connect(t, s)
	exec(t, ctx, a, "CREATE TABLE t(id int PRIMARY KEY, v int); INSERT INTO t VALUES (1, 0), (2, 0)")
	for _, c := range []*pgx.Conn{a, b} {
		exec(t, ctx, c, "SET deadlock_timeout = '100ms'")
		exec(t, ctx, c, "BEGIN")
	}
	exec(t, ctx, a, "UPDATE t SET v = 1 WHERE id = 1")
	exec(t, ctx, b, "UPDATE t SET v = 1 WHERE id = 2")

	apid := backendPID(t, ctx, a)
	doneA := async(func() error { _, err := a.Exec(ctx, "UPDATE t SET v = 2 WHERE id = 2"); return err })
	awaitLockWait(t, ctx, mon, apid)
	doneB := async(func() error { _, err := b.Exec(ctx, "UPDATE t SET v = 2 WHERE id = 1"); return err })

	errA := await(t, doneA, scaled(10*time.Second), "A's UPDATE")
	errB := await(t, doneB, scaled(10*time.Second), "B's UPDATE")
	t.Logf("A: %v; B: %v", errA, errB)
	switch {
	case sqlstate(errA) == "40P01" && errB == nil, sqlstate(errB) == "40P01" && errA == nil:
	default:
		t.Fatalf("want one 40P01 and one success; A: %v, B: %v", errA, errB)
	}
	exec(t, ctx, a, "ROLLBACK")
	exec(t, ctx, b, "ROLLBACK")
}

// timedOut runs sql on c, which must fail with 57014 (query_canceled)
// within d, and returns how long it took.
func timedOut(t *testing.T, ctx context.Context, c *pgx.Conn, sql string, d time.Duration) time.Duration {
	t.Helper()
	t0 := time.Now()
	err := await(t, async(func() error { _, err := c.Exec(ctx, sql); return err }), d, sql)
	took := time.Since(t0)
	if sqlstate(err) != "57014" {
		t.Fatalf("%s: got %v, want SQLSTATE 57014", sql, err)
	}
	t.Logf("%s: %v after %v", sql, err, took)
	return took
}

func TestStatementTimeout(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "SET statement_timeout = '300ms'")
	timedOut(t, ctx, c, "SELECT pg_sleep(10)", scaled(3*time.Second))
	exec(t, ctx, c, "SELECT 1")
}

// TestCPULoopTimeout runs without preemption, the default. The loop makes
// no host call of its own, so the SIGALRM handler runs only through
// CHECK_FOR_INTERRUPTS. CHECK_FOR_INTERRUPTS dispatches a pending signal
// it sees. Every 4096th call it also yields to the host, whose return
// dispatches pending signals too. Patch 0004 adds both paths.
func TestCPULoopTimeout(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "SET statement_timeout = '300ms'")
	timedOut(t, ctx, c, cpuLoop2e9, scaled(3*time.Second))
	exec(t, ctx, c, "SELECT 1")
}

func TestCancelBackend(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	a, b := connect(t, s), connect(t, s)
	apid := backendPID(t, ctx, a)
	done := async(func() error { _, err := a.Exec(ctx, "SELECT pg_sleep(30)"); return err })
	eventually(t, scaled(10*time.Second), "A sleeps", func() (bool, error) {
		_, event, err := waitEvent(ctx, b, apid)
		return event == "PgSleep", err
	})

	t0 := time.Now()
	var ok bool
	if err := b.QueryRow(ctx, "SELECT pg_cancel_backend($1)", apid).Scan(&ok); err != nil || !ok {
		t.Fatalf("pg_cancel_backend(%d) = %v, %v", apid, ok, err)
	}
	err := await(t, done, scaled(3*time.Second), "A's cancelled pg_sleep")
	if sqlstate(err) != "57014" {
		t.Fatalf("A: got %v, want SQLSTATE 57014", err)
	}
	t.Logf("A: %v after %v", err, time.Since(t0))
}

// TestContextCancelSendsCancelRequest checks that when a query's context
// ends, pgx sends a CancelRequest on a new connection through s.Dial, and
// the query's own connection survives. Had the cancel not arrived, pgx
// would close the connection only after DeadlineDelay (5 s), past this
// bound.
func TestContextCancelSendsCancelRequest(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	qctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	err := await(t, async(func() error {
		var v any
		return c.QueryRow(qctx, "SELECT pg_sleep(30)").Scan(&v)
	}), scaled(3*time.Second), "pg_sleep with a 200 ms context")
	if err == nil {
		t.Fatal("pg_sleep(30) with a 200 ms context succeeded")
	}
	t.Logf("query: %v after %v", err, time.Since(t0))
	if sqlstate(err) != "57014" {
		t.Fatalf("got %v, want SQLSTATE 57014 from the CancelRequest", err)
	}
	if c.IsClosed() {
		t.Fatal("the connection closed instead of being cancelled")
	}
	var one int
	if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 after cancel: %d %v", one, err)
	}
}

func TestAdvisoryLocks(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	a, b := connect(t, s), connect(t, s)
	exec(t, ctx, a, "SELECT pg_advisory_lock(42)")
	var got bool
	if err := b.QueryRow(ctx, "SELECT pg_try_advisory_lock(42)").Scan(&got); err != nil || got {
		t.Fatalf("B's pg_try_advisory_lock(42) while A holds it: %v %v", got, err)
	}
	bpid := backendPID(t, ctx, b)
	done := async(func() error { _, err := b.Exec(ctx, "SELECT pg_advisory_lock(42)"); return err })
	awaitLockWait(t, ctx, a, bpid)
	notYet(t, done, "B's pg_advisory_lock(42) while A holds it")
	if err := a.QueryRow(ctx, "SELECT pg_advisory_unlock(42)").Scan(&got); err != nil || !got {
		t.Fatalf("A's pg_advisory_unlock(42): %v %v", got, err)
	}
	if err := await(t, done, scaled(5*time.Second), "B's pg_advisory_lock(42) after A unlocks"); err != nil {
		t.Fatal(err)
	}
}

func TestListenNotify(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	a, b := connect(t, s), connect(t, s)
	exec(t, ctx, a, "LISTEN ch")
	exec(t, ctx, b, "NOTIFY ch, 'hi'")
	wctx, cancel := context.WithTimeout(ctx, scaled(5*time.Second))
	defer cancel()
	n, err := a.WaitForNotification(wctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Channel != "ch" || n.Payload != "hi" {
		t.Fatalf("notification %q %q, want ch hi", n.Channel, n.Payload)
	}
}

func TestBackendCrashRecovers(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	// The cluster runs with synchronous_commit off, and crash recovery may
	// lose commits that have not reached WAL yet.
	exec(t, ctx, c, "SET synchronous_commit = on")
	exec(t, ctx, c, "CREATE TABLE kept(v int)")
	exec(t, ctx, c, "INSERT INTO kept SELECT generate_series(1, 1000)")
	pid := backendPID(t, ctx, c)

	t0 := time.Now()
	if err := pgmem.Kernel(s).Signal(pid, sigKILL); err != nil {
		t.Fatal(err)
	}
	// Probe only once the postmaster has seen the crash, so a probe cannot
	// reach the server from before it. The log is copied asynchronously.
	killed := fmt.Sprintf("server process (PID %d) was terminated by signal 9: Killed", pid)
	eventually(t, scaled(5*time.Second), "the crash logged", func() (bool, error) {
		return strings.Contains(s.Log(), killed), nil
	})
	// Retry until recovery ends. The first attempts may get 57P03, "the
	// database system is in recovery mode".
	cfg := connConfig(t, s)
	deadline := time.Now().Add(scaled(10 * time.Second))
	var tries []string
	for {
		n, err := countKept(ctx, cfg)
		if err == nil {
			if n != 1000 {
				t.Fatalf("kept has %d rows after recovery, want 1000", n)
			}
			break
		}
		if len(tries) == 0 || tries[len(tries)-1] != err.Error() {
			tries = append(tries, err.Error())
		}
		if time.Now().After(deadline) {
			t.Fatalf("no recovery within %v; errors seen:\n%s", scaled(10*time.Second), strings.Join(tries, "\n"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("recovered after %v; errors seen before: %q", time.Since(t0), tries)

	eventually(t, scaled(5*time.Second), "recovery logged after the crash", func() (bool, error) {
		log := s.Log()
		i := strings.Index(log, killed)
		return i >= 0 && strings.Contains(log[i:], "database system is ready to accept connections"), nil
	})
}

// countKept connects, counts kept's rows and disconnects.
func countKept(ctx context.Context, cfg *pgx.ConnConfig) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, scaled(5*time.Second))
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, cfg.Copy())
	if err != nil {
		return 0, err
	}
	defer c.Close(ctx)
	var n int
	err = c.QueryRow(ctx, "SELECT count(*) FROM kept").Scan(&n)
	return n, err
}

// TestPostmasterCrash checks that when the postmaster dies, every child
// sees the postmaster-alive pipe close and exits. The listen socket goes
// with the postmaster.
func TestPostmasterCrash(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "SELECT 1")
	k := pgmem.Kernel(s)

	t0 := time.Now()
	if err := k.Signal(pgmem.PostmasterPID(s), sigKILL); err != nil {
		t.Fatal(err)
	}
	eventually(t, scaled(5*time.Second), "every process exits", func() (bool, error) {
		return k.ProcCount() == 0, nil
	})
	t.Logf("all processes gone %v after SIGKILL", time.Since(t0))

	// The backend has exited, so the connection breaks with EOF, or with
	// EPIPE on the write, and pgx closes it.
	err := await(t, async(func() error { _, err := c.Exec(ctx, "SELECT 1"); return err }),
		scaled(5*time.Second), "query on the orphaned connection")
	if err == nil {
		t.Fatal("query succeeded after the postmaster died")
	}
	if !c.IsClosed() && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("query on the orphaned connection: %v, and the connection is open; want EOF and closed", err)
	}
	t.Logf("query on the orphaned connection: %v (closed: %v)", err, c.IsClosed())
	dctx, cancel := context.WithTimeout(ctx, scaled(5*time.Second))
	defer cancel()
	if conn, err := s.Dial(dctx, "unix", ""); err == nil {
		conn.Close()
		t.Fatal("Dial succeeded after the postmaster died")
	}
	if time.Since(t0) > scaled(5*time.Second) {
		t.Fatalf("took %v", time.Since(t0))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestConnectionChurn(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	k := pgmem.Kernel(s)
	idle := k.ProcCount()
	cfg := connConfig(t, s)
	for i := range 300 {
		if err := selectOne(ctx, cfg); err != nil {
			if sqlstate(err) == "53300" {
				t.Fatalf("iteration %d: too many clients (backends are not exiting): %v", i, err)
			}
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	eventually(t, scaled(5*time.Second), fmt.Sprintf("back to %d idle processes", idle), func() (bool, error) {
		return k.ProcCount() == idle, nil
	})
}

// selectOne connects, runs SELECT 1 and disconnects.
func selectOne(ctx context.Context, cfg *pgx.ConnConfig) error {
	c, err := pgx.ConnectConfig(ctx, cfg.Copy())
	if err != nil {
		return err
	}
	var one int
	err = c.QueryRow(ctx, "SELECT 1").Scan(&one)
	return errors.Join(err, c.Close(ctx))
}

// TestGCNotStalledByQuery covers preemption off. wasm code then reaches a
// Go safe point only at a host call, so a computing backend would hold up
// every stop-the-world. CHECK_FOR_INTERRUPTS yields to the host every
// 4096th call. We measured runtime.GC at about 2 ms with the yield and
// 1.6 s without it.
func TestGCNotStalledByQuery(t *testing.T) {
	if scaled(1) > 1 {
		t.Skip("PGMEM_TIME_SCALE > 1: the 500 ms GC bound is the point of this test, so it is not scaled")
	}
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c, mon := connect(t, s), connect(t, s)
	pid := backendPID(t, ctx, c)
	done := async(func() error { _, err := c.Exec(ctx, cpuLoop30e6); return err })
	eventually(t, scaled(10*time.Second), "the query runs", func() (bool, error) {
		var state string
		err := mon.QueryRow(ctx, "SELECT state FROM pg_stat_activity WHERE pid = $1", pid).Scan(&state)
		return state == "active", err
	})
	time.Sleep(50 * time.Millisecond) // well into the computation

	t0 := time.Now()
	runtime.GC()
	gc := time.Since(t0)
	notYet(t, done, "the query (it must outlast the GC for the measurement to count)")
	t.Logf("runtime.GC during the query took %v", gc)
	if gc > 500*time.Millisecond {
		t.Fatalf("runtime.GC took %v while a backend computed, want under 500 ms", gc)
	}
	if err := await(t, done, scaled(30*time.Second), "the query"); err != nil {
		t.Fatal(err)
	}
}

// TestWALStaysBounded checks the WAL limit. pg_wal lives in the in-memory
// file system, so the cluster's max_wal_size (64 MB) must keep it from
// growing with the data. The test inserts 150 MB in three statements,
// with the select-list generate_series streaming the rows, and
// checkpoints. pg_wal must then hold at most 128 MiB.
func TestWALStaysBounded(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
	defer cancel()
	c := connect(t, s)
	var seg string
	if err := c.QueryRow(ctx, "SHOW wal_segment_size").Scan(&seg); err != nil {
		t.Fatal(err)
	}
	// With wal_level = minimal, a table created in the inserting
	// transaction would skip WAL, so create it first.
	exec(t, ctx, c, "CREATE TABLE big(v text)")
	for range 3 {
		exec(t, ctx, c, "INSERT INTO big SELECT repeat('x', 1000) FROM (SELECT generate_series(1, 50000)) s")
	}
	exec(t, ctx, c, "CHECKPOINT")
	wal, err := pgmem.DirSize(s, "/pgdata/pg_wal")
	if err != nil {
		t.Fatal(err)
	}
	var table int64
	if err := c.QueryRow(ctx, "SELECT pg_total_relation_size('big')").Scan(&table); err != nil {
		t.Fatal(err)
	}
	t.Logf("wal_segment_size %s; table %d MiB; pg_wal %d MiB after the inserts and a checkpoint", seg, table>>20, wal>>20)
	if wal > 128<<20 {
		t.Fatalf("pg_wal holds %d bytes, want at most 128 MiB", wal)
	}
	exec(t, ctx, c, "DROP TABLE big")
}

// Deep recursion. Postgres's check_stack_depth measures the C stack. Under
// WebAssembly that is only the shadow stack in linear memory. wazero's
// engine stack, where most frames keep their locals, grows far faster, and
// each active PG_TRY holds a copy of it. Patch 0008 pads each depth check
// and lowers max_stack_depth to 512kB so that the limit bounds both.
//
// These tests run recursions deep enough to matter, so each one runs under
// an rssGuard. The server runs with preemption on, so the guard's kill
// lands even in a guest that makes no host calls. The JSON test keeps the
// default, because that is the configuration whose crash it checks for.

// rssGuardLimit is the RSS past which an rssGuard stops the server.
const rssGuardLimit = 3 << 30

// processRSS returns this process's resident set size in bytes. On Linux
// it is the proportional set size from /proc/self/smaps_rollup, which
// counts the shared memory every guest process maps only once. If that
// fails, it is the RSS from /proc/self/statm. Elsewhere it comes from
// ps.
func processRSS() (int64, error) {
	if b, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "Pss:"); ok {
				kib, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
				if err != nil {
					return 0, fmt.Errorf("/proc/self/smaps_rollup: %w", err)
				}
				return kib << 10, nil
			}
		}
		return 0, fmt.Errorf("/proc/self/smaps_rollup: no Pss line")
	}
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		f := strings.Fields(string(b))
		if len(f) < 2 {
			return 0, fmt.Errorf("/proc/self/statm: %q", b)
		}
		pages, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/self/statm: %w", err)
		}
		return pages * int64(os.Getpagesize()), nil
	}
	out, err := osexec.Command("/bin/ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	return kib << 10, nil
}

// goMapped returns the memory the Go runtime has mapped and not released
// to the OS, which includes wazero's engine stacks and their copies.
// Reading it needs no fork.
func goMapped() int64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64() - s[1].Value.Uint64())
}

// rssGuard samples RSS every 50 ms. It also reads the Go runtime's mapped
// memory every 5 ms, which catches a fast allocation sooner. Its peak is
// the highest of either reading. Once either passes rssGuardLimit, it
// kills the backend pid and closes the server, so that a runaway recursion
// cannot take the machine with it. stop then fails the test.
type rssGuard struct {
	t        *testing.T
	peak     atomic.Int64
	tripped  atomic.Pointer[string]
	stopping chan struct{}
	done     chan struct{}
}

func guardRSS(t *testing.T, s *pgmem.Server, pid int32) *rssGuard {
	t.Helper()
	if _, err := processRSS(); err != nil {
		t.Fatalf("RSS guard: %v", err)
	}
	g := &rssGuard{t: t, stopping: make(chan struct{}), done: make(chan struct{})}
	trip := func(what string, v int64) {
		msg := fmt.Sprintf("%s %d MiB passed %d MiB: killed backend %d and closed the server", what, v>>20, rssGuardLimit>>20, pid)
		g.tripped.Store(&msg)
		_ = pgmem.Kernel(s).Signal(pid, sigKILL)
		go s.Close()
	}
	go func() {
		defer close(g.done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			v := goMapped()
			if v > g.peak.Load() {
				g.peak.Store(v) // a burst between two RSS samples still counts
			}
			if v > rssGuardLimit {
				trip("Go-mapped memory", v)
				return
			}
			if i%10 == 0 {
				v, err := processRSS()
				if err == nil {
					if v > g.peak.Load() {
						g.peak.Store(v)
					}
					if v > rssGuardLimit {
						trip("RSS", v)
						return
					}
				}
			}
			select {
			case <-g.stopping:
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(g.end)
	return g
}

func (g *rssGuard) end() {
	select {
	case <-g.stopping:
	default:
		close(g.stopping)
	}
	<-g.done
}

// stop ends the sampling and returns the peak of RSS and Go-mapped memory.
// It fails the test if the guard tripped.
func (g *rssGuard) stop() int64 {
	g.t.Helper()
	g.end()
	if msg := g.tripped.Load(); msg != nil {
		g.t.Fatalf("RSS guard: %s", *msg)
	}
	if v, err := processRSS(); err == nil && v > g.peak.Load() {
		g.peak.Store(v)
	}
	return g.peak.Load()
}

// crashLines are server log lines that only a backend crash produces.
var crashLines = []string{"trapped", "was terminated by signal", "terminating any other active server processes", "all server processes terminated; reinitializing"}

// TestDeepJSONIsError checks that a JSON value nested a million deep fails
// with "stack depth limit exceeded", as on native Postgres. It must not
// overflow wazero's engine stack, which would crash the backend and send
// the whole server through crash recovery.
func TestDeepJSONIsError(t *testing.T) {
	if raceEnabled {
		// Under the race detector the whole test binary sits near
		// rssGuardLimit, so the guard could trip spuriously. Plain runs
		// cover this test.
		t.Skip("race detector on: the RSS guard's limit is too close")
	}
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c, other := connect(t, s), connect(t, s)
	exec(t, ctx, other, "SELECT 1")
	pid := backendPID(t, ctx, c)

	g := guardRSS(t, s, pid)
	t0 := time.Now()
	_, err := c.Exec(ctx, "SELECT repeat('[', 1000000)::json")
	elapsed := time.Since(t0)
	peak := g.stop()
	t.Logf("deep JSON: %v after %v; peak RSS %d MiB", err, elapsed, peak>>20)
	if code := sqlstate(err); code != "54001" {
		t.Errorf("deep JSON: %v (SQLSTATE %q), want SQLSTATE 54001 (stack depth limit exceeded)", err, code)
	}
	// Both sessions live on, so no crash recovery ended them.
	exec(t, ctx, other, "SELECT 1")
	exec(t, ctx, c, "SELECT 1")
	for _, line := range crashLines {
		if strings.Contains(s.Log(), line) {
			t.Errorf("server log has %q: a backend crashed", line)
		}
	}
}

// TestRecursionMemoryBounded checks that a plpgsql function that recurses
// until Postgres stops it ends with "stack depth limit exceeded". That
// must happen before the engine-stack copies held by each level's PG_TRY
// grow the process by 1 GB.
func TestRecursionMemoryBounded(t *testing.T) {
	if scaled(1) > 1 {
		t.Skip("PGMEM_TIME_SCALE > 1: memory under CPU emulation is not the point, and the run would be slow")
	}
	if raceEnabled {
		// The race detector's shadow memory takes the test binary to
		// about 2.9 GB here, too close to rssGuardLimit to measure the
		// recursion's growth.
		t.Skip("race detector on: memory bounds are not meaningful")
	}
	s := startWith(t, pgmem.Options{}, pgmem.StartPreempt)
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "CREATE FUNCTION r(n int) RETURNS int AS $$ BEGIN RETURN r(n+1); END $$ LANGUAGE plpgsql")
	pid := backendPID(t, ctx, c)

	debug.FreeOSMemory() // return freed pages, so the baseline is what is live
	base, err := processRSS()
	if err != nil {
		t.Fatal(err)
	}
	g := guardRSS(t, s, pid)
	t0 := time.Now()
	_, err = c.Exec(ctx, "SELECT r(1)")
	elapsed := time.Since(t0)
	peak := g.stop()
	depth := 0
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) {
		depth = strings.Count(pe.Where, "PL/pgSQL function r(integer)")
	}
	t.Logf("plpgsql recursion: %d levels, %v after %v; RSS %d MiB before, peak %d MiB (+%d MiB)",
		depth, err, elapsed, base>>20, peak>>20, (peak-base)>>20)
	if code := sqlstate(err); code != "54001" {
		t.Fatalf("plpgsql recursion: %v (SQLSTATE %q), want SQLSTATE 54001 (stack depth limit exceeded)", err, code)
	}
	if peak-base >= 1<<30 {
		t.Fatalf("peak RSS grew by %d MiB during the recursion, want under 1024 MiB", (peak-base)>>20)
	}
	exec(t, ctx, c, "SELECT 1")
}

// TestModerateRecursionWorks checks that the limits that stop runaway
// recursion leave ordinary deep queries alone. These are plpgsql 100
// levels deep, an SQL function 100 levels deep, and a recursive CTE of 200
// levels.
func TestModerateRecursionWorks(t *testing.T) {
	if raceEnabled {
		// Under the race detector the whole test binary sits near
		// rssGuardLimit, so the guard could trip spuriously. Plain runs
		// cover this test.
		t.Skip("race detector on: the RSS guard's limit is too close")
	}
	s := startWith(t, pgmem.Options{}, pgmem.StartPreempt)
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "CREATE FUNCTION p(n int) RETURNS int AS $$ BEGIN IF n <= 1 THEN RETURN 1; END IF; RETURN 1 + p(n-1); END $$ LANGUAGE plpgsql")
	exec(t, ctx, c, "CREATE FUNCTION q(n int) RETURNS int AS 'SELECT CASE WHEN n <= 1 THEN 1 ELSE 1 + q(n-1) END' LANGUAGE sql")
	g := guardRSS(t, s, backendPID(t, ctx, c))
	for _, tc := range []struct {
		sql  string
		want int
	}{
		{"SELECT p(100)", 100},
		{"SELECT q(100)", 100},
		{"WITH RECURSIVE w(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM w WHERE n < 200) SELECT max(n) FROM w", 200},
	} {
		var got int
		if err := c.QueryRow(ctx, tc.sql).Scan(&got); err != nil {
			t.Errorf("%s: %v", tc.sql, err)
		} else if got != tc.want {
			t.Errorf("%s = %d, want %d", tc.sql, got, tc.want)
		}
	}
	t.Logf("peak RSS %d MiB", g.stop()>>20)
}

// TestMaxStackDepthCapped checks that patch 0008 makes 512kB both the
// default and the maximum of max_stack_depth. Its refusal does not suggest
// ulimit -s, which cannot raise the limit under pgmem.
func TestMaxStackDepthCapped(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	var v string
	if err := c.QueryRow(ctx, "SHOW max_stack_depth").Scan(&v); err != nil || v != "512kB" {
		t.Fatalf("max_stack_depth %q, %v; want 512kB", v, err)
	}
	_, err := c.Exec(ctx, "SET max_stack_depth = '1MB'")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "22023" {
		t.Fatalf("SET max_stack_depth = '1MB': %v, want SQLSTATE 22023", err)
	}
	if !strings.Contains(pe.Detail, "512kB") || strings.Contains(pe.Hint, "ulimit") || !strings.Contains(pe.Hint, "pgmem") {
		t.Errorf("detail %q, hint %q: want the 512kB cap and pgmem's hint, not ulimit -s", pe.Detail, pe.Hint)
	}
}
