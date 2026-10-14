package pgtest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem"
)

// startServer starts a pgmem server for one test and closes it at cleanup.
// It skips the test when PGMEM_BUILD_DIR names an incomplete local build,
// unless PGMEM_REQUIRE_BUILD is set.
func startServer(t *testing.T) *pgmem.Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := pgmem.Start(ctx, pgmem.Options{})
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("PGMEM_BUILD_DIR") != "" && os.Getenv("PGMEM_REQUIRE_BUILD") == "" {
		t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if t.Failed() {
			t.Logf("server log:\n%s", srv.Log())
		}
	})
	return srv
}

// dial connects to database db on srv and closes the connection at cleanup.
func dial(t *testing.T, srv *pgmem.Server, db string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, connConfig(srv, db))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// newDB starts a server, creates database tmpl, runs schema in it and
// returns a connection to it.
func newDB(t *testing.T, schema string) (*pgmem.Server, *pgx.Conn) {
	t.Helper()
	srv := startServer(t)
	admin := dial(t, srv, "postgres")
	mustExec(t, admin, "CREATE DATABASE tmpl")
	admin.Close(context.Background())
	conn := dial(t, srv, "tmpl")
	if schema != "" {
		mustExec(t, conn, schema)
	}
	return srv, conn
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func install(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if err := installSupport(context.Background(), conn); err != nil {
		t.Fatalf("installSupport: %v", err)
	}
}

// expectReset runs resetDatabase and checks its outcome.
func expectReset(t *testing.T, conn *pgx.Conn, wantOK bool, wantReason string) {
	t.Helper()
	ok, reason, err := resetDatabase(context.Background(), conn)
	if err != nil {
		t.Fatalf("resetDatabase: %v", err)
	}
	if ok != wantOK || reason != wantReason {
		t.Fatalf("resetDatabase = (%v, %q), want (%v, %q)", ok, reason, wantOK, wantReason)
	}
}

// dbState is what a reset must restore: each user table's rows (sorted
// row texts, read with ONLY) and each user sequence's position, in every
// user schema. It needs the _pgtest schema.
type dbState map[string]string

func snapshotState(t *testing.T, conn *pgx.Conn) dbState {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, `
		SELECT c.oid::regclass::text, c.relkind::text
		FROM pg_class c
		WHERE c.oid IN (SELECT _pgtest.user_relations('{r,S}'))`)
	if err != nil {
		t.Fatal(err)
	}
	type rel struct{ Name, Kind string }
	rels, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rel])
	if err != nil {
		t.Fatal(err)
	}
	st := dbState{}
	for _, r := range rels {
		var q string
		if r.Kind == "S" {
			q = "SELECT format('%s %s %s', last_value, is_called, pg_sequence_last_value(" + quoteLiteral(r.Name) + ")) FROM " + r.Name
		} else {
			q = "SELECT coalesce(string_agg(t::text, ' | ' ORDER BY t::text), '') FROM ONLY " + r.Name + " t"
		}
		var v string
		if err := conn.QueryRow(ctx, q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		st[r.Kind+" "+r.Name] = v
	}
	return st
}

const fullSchema = `
CREATE TABLE a (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, junk int, name text NOT NULL);
ALTER TABLE a DROP COLUMN junk;
INSERT INTO a (name) VALUES ('one'), ('two'), ('three');

CREATE TABLE b (id serial PRIMARY KEY, a_id int NOT NULL REFERENCES a (id), note text);
INSERT INTO b (a_id, note) VALUES (3, 'b3'), (1, 'b1'), (2, 'b2');
CREATE FUNCTION refuse() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'no inserts into b'; END$$;
CREATE TRIGGER b_refuse BEFORE INSERT ON b FOR EACH ROW EXECUTE FUNCTION refuse();

CREATE SEQUENCE counter;
SELECT setval('counter', 10);
CREATE SEQUENCE fresh START 5;
CREATE SEQUENCE once START 7;

CREATE TABLE g (x int, "Mixed Case" text, y int GENERATED ALWAYS AS (x * 2) STORED);
INSERT INTO g (x, "Mixed Case") VALUES (1, 'p'), (2, 'q');

CREATE TABLE p (k int, v text) PARTITION BY LIST (k);
CREATE TABLE p1 PARTITION OF p FOR VALUES IN (1);
CREATE TABLE p2 PARTITION OF p FOR VALUES IN (2);
INSERT INTO p VALUES (1, 'x'), (1, 'y');

CREATE UNLOGGED TABLE u (id int, v text);
INSERT INTO u VALUES (1, 'u1'), (2, 'u2');

CREATE TABLE e (id bigserial PRIMARY KEY, v text);

CREATE TABLE ip (id int);
CREATE TABLE ic () INHERITS (ip);
INSERT INTO ip VALUES (1);
INSERT INTO ic VALUES (2);

CREATE TABLE emptied (id int);
INSERT INTO emptied VALUES (1);
DELETE FROM emptied;

CREATE SCHEMA "App Schema";
CREATE TABLE "App Schema"."Odd Table" ("Key" int PRIMARY KEY, a_id int REFERENCES a (id), "Note" text);
INSERT INTO "App Schema"."Odd Table" VALUES (1, 1, 'o1'), (2, 3, 'o3');
CREATE SEQUENCE "App Schema"."Odd Seq";
SELECT setval('"App Schema"."Odd Seq"', 3);
`

const mutations = `
INSERT INTO a (name) VALUES ('four');
UPDATE a SET name = 'ONE' WHERE id = 1;
DELETE FROM b WHERE a_id IN (2, 3);
DELETE FROM "App Schema"."Odd Table" WHERE a_id = 3;
DELETE FROM a WHERE id IN (2, 3);
UPDATE b SET note = 'changed';
SELECT nextval('counter'), nextval('counter');
SELECT setval('fresh', 50, false);
SELECT nextval('once');
INSERT INTO "App Schema"."Odd Table" VALUES (3, 1, 'o-new');
UPDATE "App Schema"."Odd Table" SET "Note" = 'changed' WHERE "Key" = 1;
SELECT nextval('"App Schema"."Odd Seq"');
UPDATE g SET x = 10 WHERE x = 1;
INSERT INTO g (x, "Mixed Case") VALUES (3, 'r');
INSERT INTO p VALUES (2, 'z');
DELETE FROM p WHERE v = 'x';
DELETE FROM u;
INSERT INTO e (v) VALUES ('new'), ('newer');
INSERT INTO ip VALUES (3);
DELETE FROM ic;
INSERT INTO emptied VALUES (7);
`

func TestResetRestoresSeedsAndSequences(t *testing.T) {
	_, conn := newDB(t, fullSchema)
	install(t, conn)
	want := snapshotState(t, conn)
	if got, w := want["S once"], "7 f "; got != w {
		t.Fatalf("sequence once before the test: %q, want %q", got, w)
	}
	if _, ok := want[`r "App Schema"."Odd Table"`]; !ok {
		t.Fatalf("snapshot misses the table in schema \"App Schema\": %v", want)
	}

	var seeds []string
	rows, err := conn.Query(context.Background(), "SELECT rel::text FROM _pgtest.seeds ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	if seeds, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if wantSeeds := []string{`"App Schema"."Odd Table"`, "a", "b", "g", "ic", "ip", "p1", "u"}; !reflect.DeepEqual(seeds, wantSeeds) {
		t.Fatalf("seeds = %v, want %v", seeds, wantSeeds)
	}

	for round := range 2 {
		mustExec(t, conn, mutations)
		if got := snapshotState(t, conn); reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: mutations changed nothing: %v", round, got)
		}
		start := time.Now()
		expectReset(t, conn, true, "")
		t.Logf("round %d: _pgtest.reset() took %v", round, time.Since(start))
		if got := snapshotState(t, conn); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: after reset\n got %v\nwant %v", round, got, want)
		}
	}

	// A reset of an untouched database is cheap and still ok.
	start := time.Now()
	expectReset(t, conn, true, "")
	t.Logf("untouched: _pgtest.reset() took %v", time.Since(start))
	if got := snapshotState(t, conn); !reflect.DeepEqual(got, want) {
		t.Fatalf("after a clean reset\n got %v\nwant %v", got, want)
	}
}

func TestDDLThenReset(t *testing.T) {
	_, conn := newDB(t, "CREATE TABLE t (id int); INSERT INTO t VALUES (1)")
	install(t, conn)
	mustExec(t, conn, "CREATE TABLE x ()")
	expectReset(t, conn, false, "ddl")
}

// TestDDLGuardCannotBeBypassed checks that DDL under
// session_replication_role = replica still marks the database dirty,
// because the trigger is ENABLE ALWAYS. Disabling or dropping the trigger
// fires no event trigger, so it also checks that reset itself catches
// both.
func TestDDLGuardCannotBeBypassed(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"replica", "SET session_replication_role = replica; CREATE TABLE x ()"},
		{"disable", "ALTER EVENT TRIGGER _pgtest_ddl DISABLE"},
		{"drop", "DROP EVENT TRIGGER _pgtest_ddl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, conn := newDB(t, "")
			install(t, conn)
			mustExec(t, conn, tc.sql)
			mustExec(t, conn, "RESET session_replication_role")
			if tc.name == "replica" {
				var dirty bool
				if err := conn.QueryRow(context.Background(), "SELECT dirty FROM _pgtest.state").Scan(&dirty); err != nil || !dirty {
					t.Fatalf("dirty after DDL under replica = %v, %v; want true", dirty, err)
				}
			}
			expectReset(t, conn, false, "ddl")
		})
	}
}

func TestRolledBackDDLResets(t *testing.T) {
	_, conn := newDB(t, "CREATE TABLE t (id int); INSERT INTO t VALUES (1)")
	install(t, conn)
	mustExec(t, conn, "BEGIN")
	mustExec(t, conn, "CREATE TABLE x ()")
	mustExec(t, conn, "ROLLBACK")
	expectReset(t, conn, true, "")
}

func TestResetRefusesUnguardedState(t *testing.T) {
	t.Run("other sessions", func(t *testing.T) {
		srv, conn := newDB(t, "")
		install(t, conn)
		dial(t, srv, "tmpl")
		expectReset(t, conn, false, "other sessions")
	})
	t.Run("database settings", func(t *testing.T) {
		_, conn := newDB(t, "")
		install(t, conn)
		mustExec(t, conn, "ALTER DATABASE tmpl SET work_mem = '8MB'")
		expectReset(t, conn, false, "database settings")
	})
	t.Run("large objects", func(t *testing.T) {
		_, conn := newDB(t, "")
		install(t, conn)
		mustExec(t, conn, "SELECT lo_create(0)")
		expectReset(t, conn, false, "large objects")
	})
	// A template with large objects is never reset. Comparing counts would
	// miss a test that unlinks one and creates another, or writes into one.
	t.Run("large objects in the template", func(t *testing.T) {
		_, conn := newDB(t, "SELECT lo_from_bytea(0, 'template')")
		install(t, conn)
		expectReset(t, conn, false, "large objects")
	})
}

// TestDDLUnderAnotherRole checks that the DDL guard's function runs as its
// owner, so DDL by a role with no rights on _pgtest still works and still
// marks the database dirty.
func TestDDLUnderAnotherRole(t *testing.T) {
	_, conn := newDB(t, "CREATE ROLE pgtest_other; CREATE SCHEMA other AUTHORIZATION pgtest_other")
	install(t, conn)
	t.Cleanup(func() {
		// Roles are cluster-global, so drop it whatever the test did.
		for _, sql := range []string{"RESET ROLE", "DROP OWNED BY pgtest_other", "DROP ROLE pgtest_other"} {
			if _, err := conn.Exec(context.Background(), sql); err != nil {
				t.Errorf("%s: %v", sql, err)
			}
		}
	})
	mustExec(t, conn, "SET ROLE pgtest_other")
	mustExec(t, conn, "CREATE TABLE other.t (id int)")
	mustExec(t, conn, "RESET ROLE")
	var dirty bool
	if err := conn.QueryRow(context.Background(), "SELECT dirty FROM _pgtest.state").Scan(&dirty); err != nil || !dirty {
		t.Fatalf("dirty after DDL under another role = %v, %v; want true", dirty, err)
	}
	expectReset(t, conn, false, "ddl")
}

// TestResetRefusesChangedPgtestTables checks that a database whose test
// wrote to _pgtest's tables is dropped, not reset, because the reset
// relies on those tables. A helper that empties every schema would do
// that. Writes show in the cumulative statistics once the writing session
// ends. The statistics do not count TRUNCATE, but TRUNCATE gives the table
// a new file.
func TestResetRefusesChangedPgtestTables(t *testing.T) {
	const schema = "CREATE TABLE t (id serial PRIMARY KEY); INSERT INTO t DEFAULT VALUES"
	for _, tc := range []struct{ name, sql string }{
		{"truncate seeds", "TRUNCATE _pgtest.seeds"},
		{"truncate all", "TRUNCATE _pgtest.state, _pgtest.seeds, _pgtest.seqs"},
		{"delete seqs", "DELETE FROM _pgtest.seqs"},
		{"update seeds", "UPDATE _pgtest.seeds SET cols = cols"},
		{"same rows again", "WITH d AS (DELETE FROM _pgtest.seqs RETURNING *) INSERT INTO _pgtest.seqs SELECT * FROM d"},
		{"delete copy", "DO $$BEGIN EXECUTE format('DELETE FROM _pgtest.%I', 's' || 't'::regclass::oid); END$$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, conn := newDB(t, schema)
			install(t, conn)
			expectReset(t, conn, true, "")
			// As in a test, another session writes, then ends.
			other := dial(t, srv, "tmpl")
			mustExec(t, other, tc.sql)
			other.Close(context.Background())
			waitGone(t, conn)
			expectReset(t, conn, false, "pgtest tables changed")
		})
	}
}

// TestResetAfterInstallStatsWritten checks that the install's own writes
// to _pgtest's tables do not count as a test's once the session has
// written its statistics.
func TestResetAfterInstallStatsWritten(t *testing.T) {
	_, conn := newDB(t, "CREATE TABLE t (id serial PRIMARY KEY); INSERT INTO t DEFAULT VALUES")
	install(t, conn)
	mustExec(t, conn, "SELECT pg_stat_force_next_flush()") // written before the reply
	expectReset(t, conn, true, "")
}

// waitGone waits until database db has no session other than conn's, or
// conn's own database when db is not given.
func waitGone(t *testing.T, conn *pgx.Conn, db ...string) {
	t.Helper()
	q, args := "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()", []any{}
	if len(db) > 0 {
		q, args = "SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", []any{db[0]}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := conn.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d other sessions after 5 s", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResetEmptyDatabase(t *testing.T) {
	srv, conn := newDB(t, "")
	install(t, conn)
	expectReset(t, conn, true, "")
	expectReset(t, conn, true, "")

	// poolConfig connects through srv.Dial and sets the pool size.
	cfg := poolConfig(srv, "tmpl", 3)
	if cfg.MaxConns != 3 {
		t.Fatalf("MaxConns = %d, want 3", cfg.MaxConns)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var db string
	if err := pool.QueryRow(context.Background(), "SELECT current_database()").Scan(&db); err != nil || db != "tmpl" {
		t.Fatalf("pool: current_database() = %q, %v", db, err)
	}
}

// quoteLiteral quotes s as an SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
