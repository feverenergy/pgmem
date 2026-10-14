// Package pgtest gives each Go test its own PostgreSQL database, on a
// PostgreSQL 17 server that runs in the test process (package pgmem).
//
//	var tpl = pgtest.Template{Key: migrationsHash, Migrate: migrate}
//
//	func TestSomething(t *testing.T) {
//		t.Parallel()
//		pool := pgtest.Pool(t, tpl)
//		// pool is connected to a database of its own, copied from the
//		// template; cleanup closes it.
//	}
//
// # Servers and templates
//
// The first Pool for a Template.Key and engine starts a server for it,
// which runs until the test process exits. The server boots from an image
// of the template. That is a cluster whose database tmpl holds what
// Migrate created, plus pgtest's schema _pgtest. The image is cached on
// disk, in $PGTEST_CACHE_DIR, so Migrate runs once per Key and engine
// across test binaries and runs. The default directory is
// os.UserCacheDir()/pgmem/snapshots, and "off" turns the cache off.
// Change Key whenever Migrate's result changes, for example to a hash of
// the migration files. A cached image that does not boot is deleted and
// built again. Writing a new image deletes the images not used for 30
// days. Delete the directory to clear the cache.
//
// $PGMEM_BUILD_DIR replaces every engine with one local build. While it is
// set, templates with the same Key share one server and one cached image,
// whatever their Engine.
//
// When several test binaries need the same image at once, one builds it
// and the others wait. If that build fails, they report its error and do
// not run Migrate themselves. A cache that cannot be used, such as a
// directory that cannot be written or a relative PGTEST_CACHE_DIR, means
// building in memory, with a warning on stderr once per process.
//
// Each Pool gets a database named db_<n> and a pgxpool.Pool of up to 20
// connections to it. WithMaxConns changes the 20. The database is a new
// copy of tmpl, or a recycled one when recycling is on (see below). Tests
// can run with t.Parallel. They share the server, not their databases.
//
// # Cleanup and recycling
//
// When the test ends, cleanup closes the pool. Then:
//
//   - If the test failed, cleanup logs with t.Logf the database's server
//     log lines since Pool handed it to the test, at most 50, and every
//     line about a crashed server process. Then it drops the database.
//   - Otherwise, by default, it drops the database.
//   - With recycling on (PGTEST_RECYCLE=1), it resets the database
//     instead, restoring the template's data, and puts it on a free list
//     for a later test. If the database cannot be reset, cleanup drops
//     it. A server keeps at most parallel + 4 recycled databases, with
//     parallel as below, and drops the rest. WithoutRecycling turns
//     recycling off for one Pool even then.
//
// Recycling is off by default because a fresh copy is usually cheaper.
// BenchmarkPerTestDatabase and BenchmarkPerTestDatabaseParallel, in
// pgtest_test.go, measure both. One test at a time on a 50-table template,
// Pool and its cleanup took 6.1 ms fresh and 9.1 ms recycled. On 200
// tables it was 9.6 ms fresh and 20.3 ms recycled. Recycling can still
// win when many tests run at once. On 50 tables at 8 parallel tests it
// took 3.2 ms per test against 5.3 ms fresh.
//
// The reset truncates every table the test wrote to, reloads the
// template's rows, and moves every sequence back. Cleanup drops the
// database instead if the test:
//
//   - ran committed DDL (CREATE, ALTER, DROP of any object in it). DDL in
//     a transaction that rolled back does not count. CREATE TEMP TABLE is
//     DDL too, so a test that creates temporary tables is never recycled;
//   - wrote to pgtest's own tables in schema _pgtest, from a helper that
//     truncates every table in every schema, say. An UPDATE of
//     _pgtest.state is the exception. The DDL guard makes those updates,
//     so the reset cannot tell a test's own from them, and a test that
//     sets its dirty flag back to false gets its DDL recycled;
//   - set database-level settings (ALTER DATABASE ... SET);
//   - created large objects;
//   - left a session on the database that its pool did not open;
//   - left a connection of its pool acquired, with rows not closed or a
//     connection not released. Cleanup stops waiting for it after 5 s,
//     says so with t.Logf, and drops the database, which ends the
//     connection.
//
// Session state ends with the pool's connections. That covers prepared
// statements, SET, advisory locks and temporary tables. It cannot be
// relied on between acquires either. The pool may hand out another
// connection, and it closes a connection idle for 1 s (see below), which
// ends its temporary tables, its SET values, its session advisory locks
// and its prepared statements. A test that relies on them across
// statements must keep one connection acquired (pool.Acquire) or work in
// one transaction.
//
// pgtest neither resets nor sees changes outside the database. Roles
// (CREATE ROLE, GRANT of role membership, ALTER ROLE ... SET), ALTER
// SYSTEM, tablespaces and other databases are cluster-global. They fire
// no DDL event in the test's database, and they stay for the rest of the
// process, visible to every test on the server. Tests that need them
// should use names of their own and undo them.
//
// # Connections and memory
//
// The server's max_connections is max(500, 20*parallel + 20), capped at
// 2620. Here parallel is the -test.parallel flag, or GOMAXPROCS outside
// go test. That leaves room for one default-size pool per parallel test.
// A test with several pools counts once per pool. So does a parallel
// parent test whose parallel subtests run, because it gives up its slot
// but keeps its pool. The floor of 500 is for those. Each open connection
// is a backend of its own, about 5 MiB resident when idle. A pool opens
// connections only as the test uses them, and closes them after a second
// idle.
//
// From the first server on, a guard samples the test process's resident
// memory (PSS on Linux) every 500 ms. Past PGTEST_MAX_RSS_MB it writes a
// message and the last 50 lines of each server's log to stderr and exits
// with code 3. If it cannot read the memory 4 times in a row, it says so
// once and stops guarding.
//
// # Environment
//
//   - PGTEST_CACHE_DIR is the image cache directory, an absolute path, or
//     "off".
//   - PGTEST_RECYCLE=1 recycles the databases of tests that pass instead
//     of dropping them. Any other value, or none, drops them.
//   - PGTEST_MAX_RSS_MB is the guard's limit in MiB. The default is 4096,
//     and 0 turns the guard off.
package pgtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem"
)

// Template describes the template database that each test's database is
// copied from.
//
// Key names the schema that Migrate produces. pgtest caches the built
// template on disk under Key and the Postgres build, so change Key whenever
// Migrate's result changes. A hash of the migration files works well.
// Migrate runs once per Key and engine, against the empty database tmpl,
// through pool. A nil Migrate gives an empty template.
//
// Engine is the PostgreSQL build the template's server runs, and the zero
// Engine means pgmem.Postgres17. Templates with the same Key and different
// engines get cached images and servers of their own, unless
// $PGMEM_BUILD_DIR is set (see the package documentation).
type Template struct {
	Key     string
	Migrate func(ctx context.Context, pool *pgxpool.Pool) error
	Engine  pgmem.Engine
}

// An Option changes what Pool does.
type Option struct {
	apply func(*options)
}

type options struct {
	maxConns int32
	recycle  bool
}

// WithMaxConns sets the pool's maximum number of connections (default 20).
// n must be at least 1.
func WithMaxConns(n int32) Option {
	return Option{func(o *options) { o.maxConns = n }}
}

// WithoutRecycling makes cleanup drop the test's database instead of
// resetting it for the next test, even with PGTEST_RECYCLE=1. Without
// PGTEST_RECYCLE=1 it changes nothing, since dropping is the default.
func WithoutRecycling() Option {
	return Option{func(o *options) { o.recycle = false }}
}

// Pool returns a pool of connections to a new database for t, copied from
// tpl's template database, and registers its cleanup with t.Cleanup (see
// the package documentation). The first Pool for tpl's Key and engine
// starts the server for it, building its image if no cached one exists.
// Any failure is t.Fatal.
func Pool(t testing.TB, tpl Template, opts ...Option) *pgxpool.Pool {
	t.Helper()
	o := options{maxConns: 20, recycle: os.Getenv("PGTEST_RECYCLE") == "1"}
	for _, opt := range opts {
		if opt.apply != nil {
			opt.apply(&o)
		}
	}
	if o.maxConns < 1 {
		t.Fatalf("pgtest: WithMaxConns(%d): want at least 1", o.maxConns)
	}
	if tpl.Key == "" {
		t.Fatal("pgtest: Template.Key is empty")
	}
	s, err := getServer(t, tpl)
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.take()
	if err != nil {
		t.Fatal(err)
	}
	// The test's failure log starts here, after take. Before take, a
	// recycled database still belonged to another test.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig(s.srv, db, o.maxConns))
	if err != nil {
		s.drop(t, db)
		t.Fatalf("pgtest: open pool for database %s: %v", db, err)
	}
	t.Cleanup(func() { s.cleanup(t, pool, db, start, o.recycle) })
	return pool
}
