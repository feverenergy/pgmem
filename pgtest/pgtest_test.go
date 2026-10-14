package pgtest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem"
)

// seedTemplate has one table, items, with one seed row ('seed', id 1).
var seedTemplate = Template{
	Key: "pgtest-pool-seed",
	Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pool.Exec(ctx, `
			CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL);
			INSERT INTO items (name) VALUES ('seed')`)
		return err
	},
}

// setup gives the test a fresh cache directory and an empty registry, and
// closes the test's server at cleanup. It turns recycling on
// (PGTEST_RECYCLE=1), because most of these tests are about what cleanup
// does to a database it may recycle. TestRecyclingOptIn covers the
// default.
func setup(t *testing.T) string {
	t.Helper()
	t.Setenv("PGTEST_RECYCLE", "1")
	dir := useCacheDir(t)
	resetForTest(t)
	t.Cleanup(func() { resetForTest(t) })
	return dir
}

// fakeTB is a testing.TB that records cleanups instead of running them,
// captures what is logged, reports Failed as set, and turns Fatal into a
// recorded message and runtime.Goexit. Call Pool through runPool with it.
type fakeTB struct {
	testing.TB // the real test, for anything not overridden

	failed bool

	mu       sync.Mutex
	cleanups []func()
	out      strings.Builder
	fatal    string
}

func newFakeTB(t testing.TB) *fakeTB { return &fakeTB{TB: t} }

func (f *fakeTB) Helper() {}
func (f *fakeTB) Failed() bool {
	return f.failed
}

func (f *fakeTB) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeTB) Log(args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintln(&f.out, args...)
}

func (f *fakeTB) Logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(&f.out, format+"\n", args...)
}

func (f *fakeTB) Fatal(args ...any) {
	f.mu.Lock()
	f.fatal = fmt.Sprint(args...)
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Fatal(fmt.Sprintf(format, args...))
}

// runCleanups runs the recorded cleanups, last first, as testing does.
func (f *fakeTB) runCleanups() {
	f.mu.Lock()
	fns := f.cleanups
	f.cleanups = nil
	f.mu.Unlock()
	for _, fn := range slices.Backward(fns) {
		fn()
	}
}

func (f *fakeTB) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.String()
}

func (f *fakeTB) fatalMessage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fatal
}

// runPool calls Pool with f in a goroutine of its own, so that f.Fatal's
// Goexit ends only that goroutine. It returns nil if Pool did not return.
func runPool(f *fakeTB, tpl Template, opts ...Option) *pgxpool.Pool {
	var pool *pgxpool.Pool
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool = Pool(f, tpl, opts...)
	}()
	<-done
	return pool
}

// currentDB returns the database pool is connected to.
func currentDB(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var db string
	if err := pool.QueryRow(context.Background(), "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	return db
}

// dbExists reports whether pg_database lists db, on the server for tpl.
func dbExists(t *testing.T, tpl Template, db string) bool {
	t.Helper()
	s := serverFor(tpl)
	if s == nil {
		t.Fatalf("no server for template %q", tpl.Key)
	}
	var ok bool
	err := s.admin.QueryRow(context.Background(), "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", db).Scan(&ok)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// names returns items' names in id order.
func names(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT name FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustPoolExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// expectFree checks the free list of seedTemplate's server.
func expectFree(t *testing.T, want ...string) {
	t.Helper()
	if got := serverFor(seedTemplate).freeList(); !slices.Equal(got, want) {
		t.Fatalf("free list %q, want %q", got, want)
	}
}

func TestPoolBasic(t *testing.T) {
	setup(t)
	pool := Pool(t, seedTemplate)
	if got := pool.Config().MaxConns; got != 20 {
		t.Errorf("MaxConns %d, want 20", got)
	}
	mustPoolExec(t, pool, "INSERT INTO items (name) VALUES ('mine')")
	if got := names(t, pool); !slices.Equal(got, []string{"seed", "mine"}) {
		t.Fatalf("items %q, want [seed mine]", got)
	}
	if db := currentDB(t, pool); !strings.HasPrefix(db, "db_") {
		t.Fatalf("database %q, want db_<n>", db)
	}
}

func TestWithMaxConns(t *testing.T) {
	setup(t)
	if got := Pool(t, seedTemplate, WithMaxConns(3)).Config().MaxConns; got != 3 {
		t.Errorf("MaxConns %d, want 3", got)
	}
	f := newFakeTB(t)
	if pool := runPool(f, seedTemplate, WithMaxConns(0)); pool != nil {
		t.Fatal("WithMaxConns(0): Pool returned")
	}
	if msg := f.fatalMessage(); !strings.Contains(msg, "WithMaxConns(0)") {
		t.Fatalf("Fatal(%q), want it to name WithMaxConns(0)", msg)
	}
}

func TestPoolParallel(t *testing.T) {
	setup(t)
	t.Run("group", func(t *testing.T) {
		for i := range 8 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				pool := Pool(t, seedTemplate)
				var want []string
				for j := range 3 {
					name := fmt.Sprintf("p%d-%d", i, j)
					mustPoolExec(t, pool, "INSERT INTO items (name) VALUES ($1)", name)
					want = append(want, name)
				}
				if got := names(t, pool); !slices.Equal(got, append([]string{"seed"}, want...)) {
					t.Fatalf("items %q, want seed and %q", got, want)
				}
			})
		}
	})
	free := serverFor(seedTemplate).freeList()
	if len(free) == 0 || len(free) > 8 {
		t.Fatalf("free list %q after 8 recycled tests", free)
	}
}

// TestFreeListCapped checks that the free list holds at most
// -test.parallel + 4 databases, and that cleanup drops the ones past that.
func TestFreeListCapped(t *testing.T) {
	setParallel(t, 1)
	setup(t)
	fakes := make([]*fakeTB, 7)
	var dbs []string
	for i := range fakes {
		fakes[i] = newFakeTB(t)
		pool := runPool(fakes[i], seedTemplate)
		if pool == nil {
			t.Fatalf("Pool: %s", fakes[i].fatalMessage())
		}
		dbs = append(dbs, currentDB(t, pool))
	}
	for _, f := range fakes {
		before := len(f.output()) // the first logged the template's build
		f.runCleanups()
		if out := f.output()[before:]; out != "" {
			t.Errorf("cleanup logged %q", out)
		}
	}
	expectFree(t, dbs[:5]...)
	for _, db := range dbs[5:] {
		if dbExists(t, seedTemplate, db) {
			t.Errorf("%s survives past the free list's cap", db)
		}
	}
}

func TestDDLThenRecycle(t *testing.T) {
	setup(t)
	var dbA string
	t.Run("A", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		dbA = currentDB(t, pool)
		mustPoolExec(t, pool, "CREATE TABLE a_table (x int)")
	})
	expectFree(t)
	if dbExists(t, seedTemplate, dbA) {
		t.Fatalf("%s survives DDL", dbA)
	}
	var dbB string
	t.Run("B", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		dbB = currentDB(t, pool)
		var reg *string
		if err := pool.QueryRow(context.Background(), "SELECT to_regclass('a_table')::text").Scan(&reg); err != nil {
			t.Fatal(err)
		}
		if reg != nil {
			t.Fatalf("B (%s) sees A's table %s", dbB, *reg)
		}
	})
	if dbB == dbA {
		t.Fatalf("B got A's database %s", dbA)
	}
	expectFree(t, dbB)
}

// TestPgtestTablesWriteDiscards checks that a test that empties pgtest's
// own tables is not recycled. A helper that truncates every table in every
// schema does that.
func TestPgtestTablesWriteDiscards(t *testing.T) {
	setup(t)
	for _, sql := range []string{
		"TRUNCATE _pgtest.seeds, _pgtest.seqs",
		"DELETE FROM _pgtest.seqs",
	} {
		var db string
		t.Run(sql, func(t *testing.T) {
			pool := Pool(t, seedTemplate)
			db = currentDB(t, pool)
			mustPoolExec(t, pool, sql)
		})
		expectFree(t)
		if dbExists(t, seedTemplate, db) {
			t.Fatalf("%s: %s survives", sql, db)
		}
	}
}

func TestRolledBackDDLStillRecycles(t *testing.T) {
	setup(t)
	var dbA string
	t.Run("A", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		dbA = currentDB(t, pool)
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "CREATE TABLE rolled_back (x int)"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})
	expectFree(t, dbA)
	t.Run("B", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		if db := currentDB(t, pool); db != dbA {
			t.Fatalf("B got %s, want the recycled %s", db, dbA)
		}
	})
	expectFree(t, dbA)
}

func TestRecycledDatabaseIsClean(t *testing.T) {
	setup(t)
	var dbA string
	t.Run("A", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		dbA = currentDB(t, pool)
		mustPoolExec(t, pool, "UPDATE items SET name = 'changed'")
		mustPoolExec(t, pool, "INSERT INTO items (name) VALUES ('a1'), ('a2')")
	})
	expectFree(t, dbA)
	t.Run("B", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		if db := currentDB(t, pool); db != dbA {
			t.Fatalf("B got %s, want the recycled %s", db, dbA)
		}
		if got := names(t, pool); !slices.Equal(got, []string{"seed"}) {
			t.Fatalf("items %q, want exactly the seed", got)
		}
		var id int
		if err := pool.QueryRow(context.Background(), "INSERT INTO items (name) VALUES ('b') RETURNING id").Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != 2 {
			t.Fatalf("next id %d, want 2 (the sequence reset)", id)
		}
	})
}

// TestRecyclingOptIn checks that recycling is off unless PGTEST_RECYCLE=1,
// and that WithoutRecycling turns it off even then.
func TestRecyclingOptIn(t *testing.T) {
	setup(t)
	for _, tc := range []struct {
		name    string
		env     string // "-" leaves PGTEST_RECYCLE unset
		opts    []Option
		recycle bool
	}{
		{"default", "-", nil, false},
		{"PGTEST_RECYCLE=0", "0", nil, false},
		{"PGTEST_RECYCLE=", "", nil, false},
		{"PGTEST_RECYCLE=true", "true", nil, false},
		{"PGTEST_RECYCLE=1", "1", nil, true},
		{"PGTEST_RECYCLE=1 WithoutRecycling", "1", []Option{WithoutRecycling()}, false},
	} {
		var db string
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGTEST_RECYCLE", tc.env) // restored after the subtest
			if tc.env == "-" {
				os.Unsetenv("PGTEST_RECYCLE")
			}
			pool := Pool(t, seedTemplate, tc.opts...)
			db = currentDB(t, pool)
		})
		if got := dbExists(t, seedTemplate, db); got != tc.recycle {
			t.Fatalf("%s: %s exists %v, want %v", tc.name, db, got, tc.recycle)
		}
		if tc.recycle {
			expectFree(t, db)
			drainFree(t, serverFor(seedTemplate))
		} else {
			expectFree(t)
		}
	}
}

// recycleModes runs fn in a subtest per cleanup mode: "default", with
// PGTEST_RECYCLE unset (drop), and "PGTEST_RECYCLE=1" (recycle).
func recycleModes(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, mode := range []struct{ name, env string }{
		{"default", "-"},
		{"PGTEST_RECYCLE=1", "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("PGTEST_RECYCLE", mode.env) // restored after the subtest
			if mode.env == "-" {
				os.Unsetenv("PGTEST_RECYCLE")
			}
			fn(t)
		})
	}
}

func TestFailingTestPrintsLogAndDiscards(t *testing.T) {
	setup(t)
	recycleModes(t, func(t *testing.T) {
		f := newFakeTB(t)
		f.failed = true
		pool := runPool(f, seedTemplate)
		if pool == nil {
			t.Fatalf("Pool: %s", f.fatalMessage())
		}
		db := currentDB(t, pool)
		ctx := context.Background()
		if _, err := pool.Exec(ctx, "SELECT 1/0"); err == nil {
			t.Fatal("SELECT 1/0 succeeded")
		}
		// An error in another database stays out of this test's log.
		if _, err := serverFor(seedTemplate).admin.Exec(ctx, "SELECT 'other database'::int"); err == nil {
			t.Fatal("SELECT 'other database'::int succeeded")
		}
		f.runCleanups()

		out := f.output()
		t.Logf("captured:\n%s", out)
		header := fmt.Sprintf("pgtest: server log for database %s (most recent last):", db)
		if !strings.Contains(out, header) {
			t.Errorf("output lacks the header %q", header)
		}
		if !strings.Contains(out, " "+db+" ERROR:  division by zero") {
			t.Errorf("output lacks %s's ERROR line", db)
		}
		if !strings.Contains(out, " "+db+" STATEMENT:  SELECT 1/0") {
			t.Errorf("output lacks %s's STATEMENT line", db)
		}
		if strings.Contains(out, "other database") {
			t.Errorf("output holds another database's lines")
		}
		if dbExists(t, seedTemplate, db) {
			t.Fatalf("%s survives a failed test", db)
		}
		expectFree(t)
	})
}

func TestLeakedConnectionDoesNotHang(t *testing.T) {
	setup(t)
	old := closeTimeout
	closeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { closeTimeout = old })

	recycleModes(t, func(t *testing.T) {
		f := newFakeTB(t)
		pool := runPool(f, seedTemplate)
		if pool == nil {
			t.Fatalf("Pool: %s", f.fatalMessage())
		}
		db := currentDB(t, pool)
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release() // only now, which lets the abandoned pool.Close finish

		start := time.Now()
		f.runCleanups()
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("cleanup took %v with a leaked connection", d)
		}
		if out := f.output(); !strings.Contains(out, "connection still acquired after the test") {
			t.Errorf("cleanup logged %q, want it to say a connection was still acquired", out)
		}
		if dbExists(t, seedTemplate, db) {
			t.Fatalf("%s survives a leaked connection", db)
		}
		expectFree(t)
	})
}

// TestLingeringSessionStillRecycles covers a session that is still in
// pg_stat_activity when cleanup resets the database. A backend whose
// client just disconnected can be in that state. The reset waits for it.
func TestLingeringSessionStillRecycles(t *testing.T) {
	setup(t)
	f := newFakeTB(t)
	pool := runPool(f, seedTemplate)
	if pool == nil {
		t.Fatalf("Pool: %s", f.fatalMessage())
	}
	db := currentDB(t, pool)
	extra := dial(t, serverFor(seedTemplate).srv, db)
	go func() {
		time.Sleep(150 * time.Millisecond)
		extra.Close(context.Background())
	}()
	before := len(f.output())
	f.runCleanups()
	if out := f.output()[before:]; out != "" {
		t.Errorf("cleanup logged %q, want nothing", out)
	}
	expectFree(t, db)
}

// TestStuckSessionDiscards covers a session that stays. The reset gives up
// waiting for it, and cleanup drops the database.
func TestStuckSessionDiscards(t *testing.T) {
	setup(t)
	old := sessionsWait
	sessionsWait = 100 * time.Millisecond
	t.Cleanup(func() { sessionsWait = old })

	f := newFakeTB(t)
	pool := runPool(f, seedTemplate)
	if pool == nil {
		t.Fatalf("Pool: %s", f.fatalMessage())
	}
	db := currentDB(t, pool)
	dial(t, serverFor(seedTemplate).srv, db) // stays open until the drop ends it
	f.runCleanups()
	if dbExists(t, seedTemplate, db) {
		t.Fatalf("%s survives a session left open", db)
	}
	expectFree(t)
}

// TestSessionsCheckErrorStillResets checks that when cleanup cannot query
// the database's sessions because the admin pool fails, it says so and
// resets the database anyway. It does not take the error to mean that
// sessions remain, because the reset refuses other sessions itself.
func TestSessionsCheckErrorStillResets(t *testing.T) {
	setup(t)
	f := newFakeTB(t)
	pool := runPool(f, seedTemplate)
	if pool == nil {
		t.Fatalf("Pool: %s", f.fatalMessage())
	}
	db := currentDB(t, pool)
	s := serverFor(seedTemplate)
	pool.Close()
	waitGone(t, dial(t, s.srv, "postgres"), db)
	s.admin.Close() // the sessions check fails
	before := len(f.output())
	f.runCleanups()
	out := f.output()[before:]
	if !strings.Contains(out, "pgtest: database "+db+": cannot check its sessions") || !strings.Contains(out, "closed pool") {
		t.Errorf("cleanup logged %q, want one line saying the sessions check failed", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("cleanup logged %d lines, want 1: %q", n, out)
	}
	expectFree(t, db)
}

// TestStartFailureRemembered checks that pgtest does not retry a server
// that failed to start because Migrate failed. Every Pool for its Key
// fails with the same error, and Migrate runs once.
func TestStartFailureRemembered(t *testing.T) {
	setup(t)
	var calls atomic.Int32
	tpl := Template{Key: "pgtest-start-fails", Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		calls.Add(1)
		return fmt.Errorf("migration %d failed", 4)
	}}
	var msgs []string
	for range 2 {
		f := newFakeTB(t)
		if pool := runPool(f, tpl); pool != nil {
			t.Fatal("Pool returned with a failing Migrate")
		}
		msgs = append(msgs, f.fatalMessage())
	}
	t.Logf("Fatal: %q", msgs)
	if !strings.Contains(msgs[0], "migration 4 failed") || msgs[1] != msgs[0] {
		t.Errorf("Fatal messages %q, want the same two, naming Migrate's error", msgs)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("Migrate ran %d times, want 1", n)
	}
}

func TestCorruptCachedImageRecovers(t *testing.T) {
	dir := setup(t)
	path := imagePath(t, dir, seedTemplate.Key)
	garbage := []byte("this is not a tar file")
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeTB(t)
	pool := runPool(f, seedTemplate)
	if pool == nil {
		t.Fatalf("Pool: %s", f.fatalMessage())
	}
	defer f.runCleanups()
	if got := names(t, pool); !slices.Equal(got, []string{"seed"}) {
		t.Fatalf("items %q, want the seed", got)
	}
	out := f.output()
	t.Logf("captured:\n%s", out)
	if !strings.Contains(out, "does not boot") || !strings.Contains(out, "built template") {
		t.Errorf("output %q, want a line saying the cached image does not boot, and one saying it was built", out)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(onDisk, garbage) {
		t.Fatal("the corrupt image is still cached")
	}
	checkImage(t, onDisk)
}

// TestUndeletableCorruptImageBuilds covers a corrupt image that cannot be
// discarded because its directory is read-only. The server boots from an
// image built in memory, not from the same file again.
func TestUndeletableCorruptImageBuilds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory")
	}
	dir := setup(t)
	path := imagePath(t, dir, seedTemplate.Key)
	garbage := []byte("this is not a tar file")
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	f := newFakeTB(t)
	pool := runPool(f, seedTemplate)
	if pool == nil {
		t.Fatalf("Pool: %s", f.fatalMessage())
	}
	defer f.runCleanups()
	if got := names(t, pool); !slices.Equal(got, []string{"seed"}) {
		t.Fatalf("items %q, want the seed", got)
	}
	out := f.output()
	t.Logf("captured:\n%s", out)
	if n := strings.Count(out, "\n"); n != 1 || !strings.Contains(out, "does not boot") || !strings.Contains(out, "could not discard") {
		t.Errorf("output %q, want one line saying the image does not boot and could not be discarded", out)
	}
	if onDisk, err := os.ReadFile(path); err != nil || !bytes.Equal(onDisk, garbage) {
		t.Errorf("the read-only cache changed (%d bytes, %v)", len(onDisk), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("the read-only cache holds %d files, want 1", len(entries))
	}
}

func TestEmptyKeyFails(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", t.TempDir())
	resetForTest(t)
	t.Cleanup(func() { resetForTest(t) })
	f := newFakeTB(t)
	if pool := runPool(f, Template{}); pool != nil {
		t.Fatal("Pool returned with an empty Key")
	}
	if msg := f.fatalMessage(); msg != "pgtest: Template.Key is empty" {
		t.Fatalf("Fatal(%q), want %q", msg, "pgtest: Template.Key is empty")
	}
	if serverFor(Template{}) != nil {
		t.Fatal("an empty Key started a server")
	}
}

func TestLogLines(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", "off") // starts no server; set for the rule
	log := strings.Join([]string{
		"2026-10-14 10:00:00.001 UTC [1]  LOG:  database system is ready to accept connections",
		"2026-10-14 10:00:01.000 UTC [7] db_1 ERROR:  division by zero",
		"2026-10-14 10:00:01.000 UTC [7] db_1 STATEMENT:  SELECT 1/0",
		"2026-10-14 10:00:02.000 UTC [8] db_10 ERROR:  relation \"x\" does not exist at character 15",
		"2026-10-14 10:00:02.000 UTC [8] db_10 STATEMENT:  SELECT * FROM x",
		"\tWHERE true",
		"2026-10-14 10:00:03.000 UTC [7] db_1 ERROR:  syntax error at or near \"SELEC\" at character 1",
		"2026-10-14 10:00:03.000 UTC [7] db_1 STATEMENT:  SELEC 1",
		"\tFROM y",
		"2026-10-14 10:00:04.000 UTC [1]  LOG:  server process (PID 9) was terminated by signal 11: Segmentation fault",
		"2026-10-14 10:00:04.000 UTC [1]  DETAIL:  Failed process was running: SELECT crash()",
		"2026-10-14 10:00:05.000 UTC [10] postgres LOG:  connection received",
		"",
	}, "\n")
	got := logLines(log, "db_1", time.Time{}, 50)
	want := []string{
		"2026-10-14 10:00:01.000 UTC [7] db_1 ERROR:  division by zero",
		"2026-10-14 10:00:01.000 UTC [7] db_1 STATEMENT:  SELECT 1/0",
		"2026-10-14 10:00:03.000 UTC [7] db_1 ERROR:  syntax error at or near \"SELEC\" at character 1",
		"2026-10-14 10:00:03.000 UTC [7] db_1 STATEMENT:  SELEC 1",
		"\tFROM y",
		"2026-10-14 10:00:04.000 UTC [1]  LOG:  server process (PID 9) was terminated by signal 11: Segmentation fault",
		"2026-10-14 10:00:04.000 UTC [1]  DETAIL:  Failed process was running: SELECT crash()",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("logLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The cap keeps the most recent lines of the database. Crash lines stay
	// whatever the cap.
	var many []string
	for i := range 60 {
		many = append(many, fmt.Sprintf("2026-10-14 10:00:00.000 UTC [7] db_2 LOG:  line %d", i))
	}
	many = append(many, "2026-10-14 10:00:00.000 UTC [1]  LOG:  background writer process (PID 3) was terminated by signal 9: Killed")
	got = logLines(strings.Join(many, "\n"), "db_2", time.Time{}, 50)
	if len(got) != 51 || got[0] != many[10] || got[49] != many[59] || got[50] != many[60] {
		t.Fatalf("logLines kept %d lines: first %q, last %q", len(got), got[0], got[len(got)-1])
	}
}

// TestLogLinesSince checks that logLines leaves out entries logged before
// since, such as a recycled database's earlier tests, with their
// continuation lines. It keeps crash entries whatever their time.
func TestLogLinesSince(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", "off") // starts no server; set for the rule
	log := strings.Join([]string{
		"2026-10-14 10:00:01.000 UTC [7] db_1 ERROR:  earlier test",
		"2026-10-14 10:00:01.000 UTC [7] db_1 STATEMENT:  SELECT 'earlier'",
		"\tFROM earlier",
		"2026-10-14 10:00:01.500 UTC [1]  LOG:  server process (PID 9) was terminated by signal 11: Segmentation fault",
		"2026-10-14 10:00:01.998 UTC [7] db_1 ERROR:  within the slack",
		"2026-10-14 10:00:02.000 UTC [8] db_1 ERROR:  this test",
		"2026-10-14 10:00:02.000 UTC [8] db_1 STATEMENT:  SELECT 'this'",
		"2026-10-14 08:00:03.000 GMT [8] db_1 ERROR:  in GMT",
		"2026-10-14 12:00:04.000 +02 [8] db_1 ERROR:  at +02",
		"2026-10-14 10:00:05.000 +0130 [8] db_1 ERROR:  at +0130, so earlier",
		"2026-10-14 10:00:0x.000 UTC [8] db_1 ERROR:  bad time: kept",
	}, "\n")
	since := time.Date(2026, 10, 14, 10, 0, 2, 0, time.UTC)
	got := logLines(log, "db_1", since, 50)
	want := []string{
		"2026-10-14 10:00:01.500 UTC [1]  LOG:  server process (PID 9) was terminated by signal 11: Segmentation fault",
		"2026-10-14 10:00:01.998 UTC [7] db_1 ERROR:  within the slack",
		"2026-10-14 10:00:02.000 UTC [8] db_1 ERROR:  this test",
		"2026-10-14 10:00:02.000 UTC [8] db_1 STATEMENT:  SELECT 'this'",
		"2026-10-14 12:00:04.000 +02 [8] db_1 ERROR:  at +02",
		"2026-10-14 10:00:0x.000 UTC [8] db_1 ERROR:  bad time: kept",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("logLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestLogLinesCrashOnly checks that only the postmaster's lines about a
// process that died count as crashes. Those lines have no database in the
// prefix. Another database's line that quotes such words is not a crash.
func TestLogLinesCrashOnly(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", "off") // starts no server; set for the rule
	log := strings.Join([]string{
		"2026-10-14 10:00:01.000 UTC [7] db_2 ERROR:  invalid input syntax for type integer: \"worker was terminated by signal 9\"",
		"2026-10-14 10:00:01.000 UTC [7] db_2 STATEMENT:  SELECT 'worker was terminated by signal 9'::int",
		"2026-10-14 10:00:01.000 UTC [7] db_2 LOG:  server process (PID 1) was terminated by signal 9",
		"2026-10-14 10:00:02.000 UTC [1]  LOG:  server process (PID 9) was terminated by exception 0xC0000005",
		"2026-10-14 10:00:02.000 UTC [1]  LOG:  terminating any other active server processes",
		"2026-10-14 10:00:03.000 UTC [8] db_1 ERROR:  mine",
	}, "\n")
	got := logLines(log, "db_1", time.Time{}, 50)
	want := []string{
		"2026-10-14 10:00:01.000 UTC [7] db_2 LOG:  server process (PID 1) was terminated by signal 9",
		"2026-10-14 10:00:02.000 UTC [1]  LOG:  server process (PID 9) was terminated by exception 0xC0000005",
		"2026-10-14 10:00:03.000 UTC [8] db_1 ERROR:  mine",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("logLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestLogLinesCapSkipsContinuations checks that when the cap falls inside
// an entry, logLines leaves out the entry's remaining continuation lines
// too, so the output starts with a whole entry.
func TestLogLinesCapSkipsContinuations(t *testing.T) {
	t.Setenv("PGTEST_CACHE_DIR", "off") // starts no server; set for the rule
	log := strings.Join([]string{
		"2026-10-14 10:00:01.000 UTC [7] db_1 ERROR:  first",
		"2026-10-14 10:00:01.000 UTC [7] db_1 STATEMENT:  SELECT 1",
		"\tFROM a",
		"\tWHERE b",
		"2026-10-14 10:00:02.000 UTC [7] db_1 ERROR:  second",
		"2026-10-14 10:00:02.000 UTC [7] db_1 STATEMENT:  SELECT 2",
	}, "\n")
	got := logLines(log, "db_1", time.Time{}, 4)
	want := []string{
		"2026-10-14 10:00:02.000 UTC [7] db_1 ERROR:  second",
		"2026-10-14 10:00:02.000 UTC [7] db_1 STATEMENT:  SELECT 2",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("logLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestFailureLogOmitsEarlierTests checks that a failing test's log holds
// only its own lines, although a recycled database serves many tests. A
// logs an error and passes. B gets A's database, logs another error and
// fails.
func TestFailureLogOmitsEarlierTests(t *testing.T) {
	setup(t)
	var dbA string
	t.Run("A", func(t *testing.T) {
		pool := Pool(t, seedTemplate)
		dbA = currentDB(t, pool)
		if _, err := pool.Exec(context.Background(), "SELECT 'from test A'::int"); err == nil {
			t.Fatal("a bad cast succeeded")
		}
	})
	expectFree(t, dbA)

	f := newFakeTB(t)
	f.failed = true
	pool := runPool(f, seedTemplate)
	if pool == nil {
		t.Fatalf("Pool: %s", f.fatalMessage())
	}
	if db := currentDB(t, pool); db != dbA {
		t.Fatalf("B got %s, want A's recycled %s", db, dbA)
	}
	if _, err := pool.Exec(context.Background(), "SELECT 'from test B'::int"); err == nil {
		t.Fatal("a bad cast succeeded")
	}
	f.runCleanups()
	out := f.output()
	t.Logf("captured:\n%s", out)
	if !strings.Contains(out, "from test B") {
		t.Errorf("B's log lacks its own error")
	}
	if strings.Contains(out, "from test A") {
		t.Errorf("B's log holds A's error")
	}
}

// TestFailureLogStartsAtTake checks that a failing test's log starts when
// its Pool takes the database, not when Pool is called. B calls Pool while
// A still holds the database that B then gets, recycled. What A logs on it
// in between is not B's.
func TestFailureLogStartsAtTake(t *testing.T) {
	setup(t)
	ctx := context.Background()
	a := newFakeTB(t)
	poolA := runPool(a, seedTemplate)
	if poolA == nil {
		t.Fatalf("Pool for A: %s", a.fatalMessage())
	}
	dbA := currentDB(t, poolA)
	s := serverFor(seedTemplate)

	// B's Pool waits in getServer, on registry.mu, until A is done.
	b := newFakeTB(t)
	b.failed = true
	registry.mu.Lock()
	var unlock sync.Once
	defer unlock.Do(registry.mu.Unlock)
	result := make(chan *pgxpool.Pool, 1)
	go func() { result <- runPool(b, seedTemplate) }()
	time.Sleep(100 * time.Millisecond) // B's Pool call has started

	if _, err := poolA.Exec(ctx, "SELECT 'from test A'::int"); err == nil {
		t.Fatal("a bad cast succeeded")
	}
	a.runCleanups()
	if got := s.freeList(); !slices.Equal(got, []string{dbA}) {
		t.Fatalf("free list %q after A, want %q", got, dbA)
	}
	time.Sleep(20 * time.Millisecond) // past logSlack
	unlock.Do(registry.mu.Unlock)

	poolB := <-result
	if poolB == nil {
		t.Fatalf("Pool for B: %s", b.fatalMessage())
	}
	if db := currentDB(t, poolB); db != dbA {
		t.Fatalf("B got %s, want A's recycled %s", db, dbA)
	}
	if _, err := poolB.Exec(ctx, "SELECT 'from test B'::int"); err == nil {
		t.Fatal("a bad cast succeeded")
	}
	b.runCleanups()
	out := b.output()
	t.Logf("captured:\n%s", out)
	if !strings.Contains(out, "from test B") {
		t.Errorf("B's log lacks its own error")
	}
	if strings.Contains(out, "from test A") {
		t.Errorf("B's log holds A's error, logged after B's Pool call but before B took the database")
	}
}

// benchTemplateOf returns a template of n tables, every tenth with 10
// seed rows.
func benchTemplateOf(n int) Template {
	return Template{
		Key: fmt.Sprintf("pgtest-bench-%d", n),
		Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
			var sql strings.Builder
			for i := range n {
				fmt.Fprintf(&sql, "CREATE TABLE t%d (id serial PRIMARY KEY, name text NOT NULL, n int);\n", i)
				if i%10 == 0 {
					fmt.Fprintf(&sql, "INSERT INTO t%d (name, n) SELECT 'seed ' || g, g FROM generate_series(1, 10) g;\n", i)
				}
			}
			_, err := pool.Exec(ctx, sql.String())
			return err
		},
	}
}

// benchTemplate has 50 tables, 5 of them with 10 seed rows each.
var benchTemplate = benchTemplateOf(50)

// benchUse is a test's work in the benchmark: a few writes and a read.
func benchUse(b *testing.B, pool *pgxpool.Pool) {
	if err := benchWork(context.Background(), pool); err != nil {
		b.Fatal(err)
	}
}

// benchWork is benchUse's work, on anything that runs queries, with an
// error instead of b.Fatal, for goroutines.
func benchWork(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	for _, table := range []string{"t0", "t1", "t2"} {
		if _, err := q.Exec(ctx, "INSERT INTO "+table+" (name, n) SELECT 'row ' || g, g FROM generate_series(1, 5) g"); err != nil {
			return err
		}
	}
	var n int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM t0").Scan(&n); err != nil || n != 15 {
		return fmt.Errorf("count(*) = %d, %v; want 15", n, err)
	}
	return nil
}

// BenchmarkPerTestDatabase measures a test database's overhead for each
// CREATE DATABASE strategy. It compares a new database per test (create,
// use, drop) with a recycled one (use, reset, after one create). Then it
// measures Pool and its cleanup, recycled and fresh. Run it with
// -run '^$' -bench PerTestDatabase -benchtime=50x.
func BenchmarkPerTestDatabase(b *testing.B) {
	b.Setenv("PGTEST_CACHE_DIR", b.TempDir())
	b.Setenv("PGTEST_RECYCLE", "1") // "recycled" passes no option
	resetForTest(b)
	b.Cleanup(func() { resetForTest(b) })
	f := newFakeTB(b)
	if pool := runPool(f, benchTemplate); pool == nil {
		b.Fatalf("Pool: %s", f.fatalMessage())
	}
	f.runCleanups()
	s := serverFor(benchTemplate)
	ctx := context.Background()
	open := func(b *testing.B, db string) *pgxpool.Pool {
		pool, err := pgxpool.NewWithConfig(ctx, poolConfig(s.srv, db, 20))
		if err != nil {
			b.Fatal(err)
		}
		return pool
	}
	n := 0
	for _, strategy := range []string{"FILE_COPY", "WAL_LOG"} {
		create := func(b *testing.B) string {
			n++
			db := fmt.Sprintf("bench_%d", n)
			if _, err := s.admin.Exec(ctx, "CREATE DATABASE "+db+" TEMPLATE tmpl STRATEGY "+strategy); err != nil {
				b.Fatal(err)
			}
			return db
		}
		drop := func(b *testing.B, db string) {
			if _, err := s.admin.Exec(ctx, "DROP DATABASE "+db+" WITH (FORCE)"); err != nil {
				b.Fatal(err)
			}
		}
		b.Run(strategy+"/create", func(b *testing.B) {
			for b.Loop() {
				db := create(b)
				b.StopTimer()
				drop(b, db)
				b.StartTimer()
			}
		})
		b.Run(strategy+"/create+use+drop", func(b *testing.B) {
			for b.Loop() {
				db := create(b)
				pool := open(b, db)
				benchUse(b, pool)
				pool.Close()
				s.waitIdle(db)
				drop(b, db)
			}
		})
		b.Run(strategy+"/use+reset", func(b *testing.B) {
			db := create(b)
			defer drop(b, db)
			for b.Loop() {
				pool := open(b, db)
				benchUse(b, pool)
				pool.Close()
				s.waitIdle(db)
				if ok, reason, err := s.reset(db); !ok || err != nil {
					b.Fatalf("reset: %v %q %v", ok, reason, err)
				}
			}
		})
	}
	for _, mode := range []struct {
		name string
		opts []Option
	}{{"Pool/recycled", nil}, {"Pool/fresh", []Option{WithoutRecycling()}}} {
		b.Run(mode.name, func(b *testing.B) {
			for b.Loop() {
				f := newFakeTB(b)
				pool := runPool(f, benchTemplate, mode.opts...)
				if pool == nil {
					b.Fatalf("Pool: %s", f.fatalMessage())
				}
				benchUse(b, pool)
				f.runCleanups()
			}
		})
	}
}

// BenchmarkPerTestDatabaseParallel measures Pool and its cleanup, recycled
// and fresh (WithoutRecycling), with 1, 8 and 16 tests at a time, on
// templates of 50 and 200 tables. One op is a batch of 200 test databases,
// each running benchUse. A batch starts from an empty free list, as in a
// new test binary, and the timer leaves out the server start. ms/testdb is
// the batch's wall time divided by 200, and creates/batch counts CREATE
// DATABASE.
//
// The reset sub-benchmarks split cleanup's reset into its connect and its
// SQL (_pgtest.reset), at the same parallelism. Each worker repeats
// connect, benchUse's writes, reset and close on a database of its own,
// 200 times in all. ms/reset is the wall time divided by 200. connect-ms
// and sql-ms are the mean latency of each part, as one worker sees it.
//
// Run it with -run '^$' -bench PerTestDatabaseParallel -benchtime=3x.
func BenchmarkPerTestDatabaseParallel(b *testing.B) {
	const batch = 200
	b.Setenv("PGTEST_CACHE_DIR", b.TempDir())
	b.Setenv("PGTEST_RECYCLE", "1") // "recycled" passes no option
	for _, tables := range []int{50, 200} {
		tpl := benchTemplateOf(tables)
		resetForTest(b) // one server at a time
		f := newFakeTB(b)
		if pool := runPool(f, tpl); pool == nil {
			b.Fatalf("Pool: %s", f.fatalMessage())
		}
		f.runCleanups()
		s := serverFor(tpl)
		for _, par := range []int{1, 8, 16} {
			for _, mode := range []struct {
				name string
				opts []Option
			}{{"recycled", nil}, {"fresh", []Option{WithoutRecycling()}}} {
				b.Run(fmt.Sprintf("tables=%d/parallel=%d/%s", tables, par, mode.name), func(b *testing.B) {
					var creates int
					for b.Loop() {
						b.StopTimer()
						drainFree(b, s)
						before := s.created()
						b.StartTimer()
						if err := runBatch(b, tpl, par, batch, mode.opts); err != nil {
							b.Fatal(err)
						}
						creates += s.created() - before
					}
					b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/float64(b.N*batch), "ms/testdb")
					b.ReportMetric(float64(creates)/float64(b.N), "creates/batch")
				})
			}
			b.Run(fmt.Sprintf("tables=%d/parallel=%d/reset", tables, par), func(b *testing.B) {
				drainFree(b, s)
				dbs := make([]string, par)
				for i := range dbs {
					db, err := s.take()
					if err != nil {
						b.Fatal(err)
					}
					dbs[i] = db
				}
				var connect, reset atomic.Int64 // nanoseconds, summed
				for b.Loop() {
					if err := resetBatch(s, dbs, batch, &connect, &reset); err != nil {
						b.Fatal(err)
					}
				}
				n := float64(b.N * batch)
				b.ReportMetric(float64(b.Elapsed().Microseconds())/1000/n, "ms/reset")
				b.ReportMetric(float64(connect.Load())/1e6/n, "connect-ms")
				b.ReportMetric(float64(reset.Load())/1e6/n, "sql-ms")
				for _, db := range dbs {
					s.drop(b, db)
				}
			})
		}
		drainFree(b, s)
	}
	resetForTest(b)
}

// created returns how many databases s has created.
func (s *server) created() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// drainFree drops the databases on s's free list.
func drainFree(b testing.TB, s *server) {
	s.mu.Lock()
	free := s.free
	s.free = nil
	s.mu.Unlock()
	for _, db := range free {
		s.drop(b, db)
	}
}

// runBatch runs total test databases, par at a time. Each one runs Pool,
// benchWork and Pool's cleanup. A database that cleanup logs about is an
// error.
func runBatch(b *testing.B, tpl Template, par, total int, opts []Option) error {
	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, par)
	for range par {
		wg.Go(func() {
			for next.Add(1) <= int64(total) {
				f := newFakeTB(b)
				pool := runPool(f, tpl, opts...)
				if pool == nil {
					errs <- fmt.Errorf("Pool: %s", f.fatalMessage())
					return
				}
				err := benchWork(context.Background(), pool)
				f.runCleanups()
				if err == nil && f.output() != "" {
					err = fmt.Errorf("cleanup logged %q", f.output())
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	return <-errs
}

// resetBatch runs total resets across dbs, one worker per database. Each
// worker connects, writes, resets and closes, and adds the connect's and
// the reset's durations to connect and reset.
func resetBatch(s *server, dbs []string, total int, connect, reset *atomic.Int64) error {
	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, len(dbs))
	for _, db := range dbs {
		wg.Go(func() {
			ctx := context.Background()
			for next.Add(1) <= int64(total) {
				t0 := time.Now()
				conn, err := pgx.ConnectConfig(ctx, connConfig(s.srv, db))
				if err != nil {
					errs <- err
					return
				}
				connect.Add(int64(time.Since(t0)))
				err = benchWork(ctx, conn)
				if err == nil {
					t1 := time.Now()
					var ok bool
					var reason string
					ok, reason, err = resetDatabase(ctx, conn)
					reset.Add(int64(time.Since(t1)))
					if err == nil && !ok {
						err = fmt.Errorf("reset refused %s: %s", db, reason)
					}
				}
				conn.Close(ctx)
				if err == nil {
					var idle bool
					if idle, err = s.waitIdle(db); err == nil && !idle {
						err = fmt.Errorf("%s: a session stayed after close", db)
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	return <-errs
}

// Two templates with the same Key and different engines get their own
// cached images and their own servers. The second engine is Postgres17
// under another Version, so it boots too. The first server is closed
// before the second starts but stays in the registry, so the process
// runs one server at a time, and a registry keyed by Key alone would
// hand the second template the closed server.
func TestTemplateEnginesKeptApart(t *testing.T) {
	t.Setenv("PGMEM_BUILD_DIR", "") // it would replace both engines
	dir := setup(t)
	other := pgmem.Postgres17
	other.Version = "17.11-test"
	tplA := Template{Key: "pgtest-engines", Migrate: seedTemplate.Migrate}
	tplB := tplA
	tplB.Engine = other

	use := func(tpl Template) *server {
		t.Helper()
		f := newFakeTB(t)
		pool := runPool(f, tpl)
		if pool == nil {
			t.Fatalf("Pool for engine %q: %s", tpl.Engine.Version, f.fatalMessage())
		}
		if got := names(t, pool); !slices.Equal(got, []string{"seed"}) {
			t.Fatalf("engine %q: items %q, want [seed]", tpl.Engine.Version, got)
		}
		f.runCleanups()
		return serverFor(tpl)
	}
	a := use(tplA)
	if serverFor(tplB) != nil {
		t.Fatal("the registry has a server for the second engine before its first Pool")
	}
	closeServers()
	b := use(tplB)
	if a == nil || b == nil || a == b {
		t.Fatalf("servers %p and %p, want two", a, b)
	}

	pathA, pathB := templatePath(t, dir, tplA), templatePath(t, dir, tplB)
	if pathA == pathB {
		t.Fatalf("both engines cache their image as %s", pathA)
	}
	for _, p := range []string{pathA, pathB} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
}
