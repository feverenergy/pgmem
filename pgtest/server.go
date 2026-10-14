package pgtest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem"
	"github.com/feverenergy/pgmem/internal/buildid"
)

// createStrategy is the CREATE DATABASE strategy for per-test databases.
// FILE_COPY copies the template's files; WAL_LOG reads and writes them
// block by block through shared buffers and logs every block.
// BenchmarkPerTestDatabase (-benchtime=50x, Apple M5 Pro, darwin/arm64), on
// a template of 50 tables with 10 seed rows in 5 of them, in ms per test
// database:
//
//	STRATEGY   create   create+use+drop   use+reset
//	FILE_COPY     2.3               6.1         9.2
//	WAL_LOG      12.8              16.9         9.4
//
// Pool and its cleanup take 6.1 ms fresh (the default) and 9.1 ms
// recycled (PGTEST_RECYCLE=1). Run in parallel, fresh databases stop
// scaling at about 5.3 ms each, because every FILE_COPY create waits for
// two immediate checkpoints and the one checkpointer runs them in turn. In
// BenchmarkPerTestDatabaseParallel (batches of 200 test databases; the
// second of two runs), in wall-clock ms per test database:
//
//	TABLES  PARALLEL  RECYCLED  FRESH
//	    50         1       9.8    6.4
//	    50         8       3.2    5.3
//	    50        16       3.8    5.3
//	   200         1      20.3    9.6
//	   200         8       8.6    9.6
//	   200        16      10.4    9.8
//
// Recycling is off by default, and PGTEST_RECYCLE=1 turns it on.
const createStrategy = "FILE_COPY"

// Timeouts and waits for a server and for a test's cleanup. Some are
// variables only so that tests can shorten them.
const (
	startTimeout = 2 * time.Minute  // a server's start from an image
	dbTimeout    = 30 * time.Second // CREATE DATABASE, DROP DATABASE, a reset
)

var (
	// closeTimeout bounds the close of a test's pool, which waits for
	// every acquired connection to be released.
	closeTimeout = 5 * time.Second
	// sessionsWait bounds how long cleanup waits for a test's database to
	// have no sessions. pg_stat_activity still lists a backend for a
	// moment after its client disconnects.
	sessionsWait = time.Second
)

// testParallel is -test.parallel, the number of parallel tests that run
// at once. Without a parsed -test.parallel flag (outside go test), it is
// GOMAXPROCS, the flag's default.
func testParallel() int {
	if f := flag.Lookup("test.parallel"); f != nil && flag.Parsed() {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return runtime.GOMAXPROCS(0)
}

// maxConnectionsCap is the upper bound of maxConnections.
// TestMaxConnectionsAtCap starts a server at it on every run. It is the
// formula's value at -test.parallel=130.
const maxConnectionsCap = 2620

// maxConnections is the server's max_connections. It leaves room for one
// pool of the default size (20) per parallel test, plus 20 for the admin
// pool, resets and slack. It is never under 500 and never over
// maxConnectionsCap. The floor is for parent tests. A parallel test that
// runs parallel subtests gives up its slot while they run but keeps its
// pool, so more pools than -test.parallel can be open. Their idle
// connections close after a second (poolConfig).
func maxConnections() int {
	return min(maxConnectionsCap, max(500, 20*testParallel()+20))
}

// freeListCap is how many recycled databases a server keeps for the next
// tests. That is one per parallel test, plus a few more.
func freeListCap() int { return testParallel() + 4 }

// A server is the registry's entry for one template on one Postgres build
// (serverKey). It is a Postgres server booted from the template's image,
// plus the databases copied from tmpl on it.
type server struct {
	engine string // engineName of the template's engine, for messages
	once   sync.Once
	err    error // start's error, for every Pool that asks
	srv    *pgmem.Server
	admin  *pgxpool.Pool // on database postgres

	// mu guards free and n, and srv for the RSS guard, which may read it
	// while start sets it.
	mu      sync.Mutex
	free    []string // recycled databases, ready for a test; the last is used first
	freeCap int      // the most free may hold (freeListCap at start)
	n       int      // the number of databases created: db_1 … db_<n>
}

// A serverKey names a template's server: its Template.Key and the build
// ID of its engine (buildid.ID). Two templates with the same Key and
// different engines get servers of their own.
type serverKey struct {
	key     string
	buildID string
}

// serverKeyOf returns tpl's serverKey.
func serverKeyOf(tpl Template) (serverKey, error) {
	id, err := buildid.ID(tpl.Engine.Version, tpl.Engine.FS)
	if err != nil {
		return serverKey{}, fmt.Errorf("pgtest: template %q: %w", tpl.Key, err)
	}
	return serverKey{tpl.Key, id}, nil
}

// engineName names engine e in messages: its Version, or the local build
// when $PGMEM_BUILD_DIR replaces every engine.
func engineName(e pgmem.Engine) string {
	if dir := os.Getenv("PGMEM_BUILD_DIR"); dir != "" {
		return "PGMEM_BUILD_DIR=" + dir
	}
	if e.Version == "" && e.FS == nil {
		return pgmem.Postgres17.Version
	}
	return e.Version
}

// registry holds the servers, by serverKey. They run until the process
// exits.
var registry struct {
	mu      sync.Mutex
	servers map[serverKey]*server
}

// getServer returns the started server for tpl and starts it on first
// use. With the process's first server, it also starts the RSS guard
// (guard.go). getServer remembers a failed start, and every later call
// returns its error. Lines about the template's image (built, unusable,
// rebuilt) go to the t.Logf of the test that started the server.
func getServer(t testing.TB, tpl Template) (*server, error) {
	key, err := serverKeyOf(tpl)
	if err != nil {
		return nil, err
	}
	registry.mu.Lock()
	s := registry.servers[key]
	if s == nil {
		s = &server{engine: engineName(tpl.Engine)}
		if registry.servers == nil {
			registry.servers = map[serverKey]*server{}
		}
		registry.servers[key] = s
	}
	registry.mu.Unlock()
	s.once.Do(func() {
		startGuard()
		s.err = fmt.Errorf("pgtest: start server for template %q: did not finish", tpl.Key) // a panic or Goexit in start
		s.err = s.start(tpl, t.Logf)
	})
	return s, s.err
}

// start boots s's server from tpl's image and opens its admin pool. If a
// cached image does not boot, it discards it and boots from a new build.
func (s *server) start(tpl Template, logf func(string, ...any)) error {
	built := false // templateImage calls logf exactly when it builds
	logBuild := func(format string, args ...any) {
		built = true
		logf(format, args...)
	}
	img, err := templateImage(context.Background(), tpl, logBuild)
	if err != nil {
		return err
	}
	srv, err := boot(img, tpl.Engine)
	if err != nil && !built {
		why := strings.SplitN(err.Error(), "\n", 2)[0]
		if derr := discardCached(tpl); derr != nil {
			// templateImage would return the same file, so build without it.
			logf("pgtest: cached image for template %q does not boot (%s); could not discard it (%v); building in memory", tpl.Key, why, derr)
			img, err = buildTemplate(tpl)
		} else {
			logf("pgtest: cached image for template %q does not boot (%s); discarded it", tpl.Key, why)
			img, err = templateImage(context.Background(), tpl, logf)
		}
		if err != nil {
			return err
		}
		srv, err = boot(img, tpl.Engine)
	}
	if err != nil {
		return fmt.Errorf("pgtest: start server for template %q: %w", tpl.Key, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, poolConfig(srv, "postgres", 4))
	if err != nil {
		srv.Close()
		return fmt.Errorf("pgtest: start server for template %q: %w", tpl.Key, err)
	}
	s.mu.Lock() // the RSS guard reads srv under mu
	s.srv, s.admin = srv, admin
	s.freeCap = freeListCap()
	s.mu.Unlock()
	return nil
}

// boot starts a server on engine e from img.
func boot(img []byte, e pgmem.Engine) (*pgmem.Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	return pgmem.Start(ctx, pgmem.Options{
		Engine:   e,
		Image:    img,
		Settings: map[string]string{"max_connections": strconv.Itoa(maxConnections())},
	})
}

// close closes s's admin pool and server, if it started.
func (s *server) close() {
	if s.srv == nil {
		return
	}
	s.admin.Close()
	s.srv.Close()
}

// take returns a database for a test. That is a recycled one if there is
// one, and otherwise a new copy of tmpl.
func (s *server) take() (string, error) {
	s.mu.Lock()
	if n := len(s.free); n > 0 {
		db := s.free[n-1]
		s.free = s.free[:n-1]
		s.mu.Unlock()
		return db, nil
	}
	s.n++
	db := "db_" + strconv.Itoa(s.n)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if _, err := s.admin.Exec(ctx, "CREATE DATABASE "+db+" TEMPLATE tmpl STRATEGY "+createStrategy); err != nil {
		return "", fmt.Errorf("pgtest: create database %s: %w", db, err)
	}
	return db, nil
}

// cleanup ends a test's use of database db, whose pool is pool. It closes
// the pool and waits for the database's sessions to end. Then it recycles
// db if recycle is set, the test passed and the reset succeeds. Otherwise,
// or when the free list is full, it drops db. When the test failed, it
// first logs db's server log lines from start on, which is when the
// test's Pool took db. Earlier tests of a recycled database logged to it
// too.
func (s *server) cleanup(t testing.TB, pool *pgxpool.Pool, db string, start time.Time, recycle bool) {
	discard := "" // why db is dropped
	closed := make(chan struct{})
	go func() {
		// If a connection is never released, this goroutine stays
		// blocked for the life of the process; the drop below ends the
		// connection's backend.
		pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
		idle, err := s.waitIdle(db)
		switch {
		case err != nil:
			// Not a session that stays. The reset refuses those itself.
			t.Logf("pgtest: database %s: cannot check its sessions (%v); resetting it all the same", db, err)
		case !idle:
			discard = "other sessions"
		}
	case <-time.After(closeTimeout):
		discard = "connection still acquired after the test"
		t.Logf("pgtest: database %s: %s (rows or a connection not released?) after %v; dropping it", db, discard, closeTimeout)
	}

	switch {
	case t.Failed():
		lines := logLines(s.srv.Log(), db, start, maxLogLines)
		t.Logf("pgtest: server log for database %s (most recent last):\n%s", db, strings.Join(lines, "\n"))
		discard = "test failed"
	case discard != "":
	case !recycle:
		discard = "recycling off"
	default:
		ok, reason, err := s.reset(db)
		if err != nil {
			t.Logf("pgtest: database %s: %v; dropping it", db, err)
			discard = "reset failed"
		} else if !ok {
			discard = reason // not logged, because a dirty database is not an error
		}
	}
	if discard == "" {
		s.mu.Lock()
		if len(s.free) < s.freeCap {
			s.free = append(s.free, db)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock() // the free list is full: more databases than tests to come
	}
	s.drop(t, db)
}

// waitIdle waits up to sessionsWait until db has no sessions, and reports
// whether that happened. The backends of a pool that has just closed take
// a moment to exit. Until they do, the reset refuses the database ("other
// sessions"), and DROP DATABASE sleeps 100 ms between its checks for them.
// err is the admin query's error. When time runs out, waitIdle cannot
// tell, and it returns false with no error.
func (s *server) waitIdle(db string) (idle bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionsWait)
	defer cancel()
	for pause := time.Millisecond; ; pause = min(2*pause, 50*time.Millisecond) {
		err := s.admin.QueryRow(ctx, "SELECT NOT EXISTS (SELECT FROM pg_stat_activity WHERE datname = $1)", db).Scan(&idle)
		switch {
		case err != nil && ctx.Err() != nil:
			return false, nil
		case err != nil:
			return false, err
		case idle:
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(pause):
		}
	}
}

// reset restores the template's data in db, on a connection of its own,
// which must be db's only session.
func (s *server) reset(db string) (ok bool, reason string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, connConfig(s.srv, db))
	if err != nil {
		return false, "", fmt.Errorf("pgtest: reset: %w", err)
	}
	defer conn.Close(ctx)
	return resetDatabase(ctx, conn)
}

// drop drops db and ends its sessions. It logs a failure instead of
// failing the test, because the test itself is done.
func (s *server) drop(t testing.TB, db string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if _, err := s.admin.Exec(ctx, "DROP DATABASE "+db+" WITH (FORCE)"); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w (after %v)", err, dbTimeout)
		}
		t.Logf("pgtest: drop database %s: %v", db, err)
	}
}
