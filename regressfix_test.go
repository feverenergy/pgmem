package pgmem_test

import (
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/feverenergy/pgmem"
)

// A failed check of a string setting is an ERROR; the session lives on.
// wazero v1.12.0 reverted wasm locals in catch handlers, which turned these
// into FATAL until patch 0005; the pinned wazero fixes it too.
//
// The last case is the role check hook: a non-superuser may not SET ROLE
// to a role it is not a member of.
func TestSettingCheckFailureIsError(t *testing.T) {
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	ctx := testCtx(t)
	exec(t, ctx, c, "CREATE ROLE alice")
	exec(t, ctx, c, "CREATE ROLE bob")
	for _, tc := range []struct{ before, q string }{
		{"", "SET datestyle = 'bogus'"},
		{"", "SET default_table_access_method = ''"},
		{"", "SET default_text_search_config = no_such_config"},
		{"SET SESSION AUTHORIZATION alice", "SET ROLE bob"},
	} {
		if tc.before != "" {
			exec(t, ctx, c, tc.before)
		}
		_, err := c.Exec(ctx, tc.q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Severity != "ERROR" {
			t.Fatalf("%s: %v, want an ERROR", tc.q, err)
		}
		var user string
		if err := c.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil {
			t.Fatalf("session dead after %s: %v", tc.q, err)
		}
		want := "postgres"
		if tc.before != "" {
			want = "alice"
		}
		if user != want {
			t.Errorf("after %s: current_user %q, want %q", tc.q, user, want)
		}
	}
}

// Encodings other than UTF-8 work: the conversion procs are linked.
func TestClientEncodingLatin1(t *testing.T) {
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	ctx := testCtx(t)
	var b []byte
	if err := c.QueryRow(ctx, "SELECT convert_to('é', 'LATIN1')").Scan(&b); err != nil || len(b) != 1 || b[0] != 0xe9 {
		t.Fatalf("convert_to: %x %v", b, err)
	}
	if _, err := c.Exec(ctx, "SET client_encoding = 'LATIN1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "RESET client_encoding"); err != nil {
		t.Fatal(err)
	}
}

// Every default conversion in pg_conversion runs: each one loads its
// library ($libdir/<proc>), which must be linked into postgres.wasm.
func TestEveryDefaultConversion(t *testing.T) {
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	ctx := testCtx(t)
	rows, err := c.Query(ctx, `SELECT c.conname, pg_encoding_to_char(c.conforencoding), pg_encoding_to_char(c.contoencoding), p.probin
		FROM pg_conversion c JOIN pg_proc p ON p.oid = c.conproc WHERE c.condefault ORDER BY c.conname`)
	if err != nil {
		t.Fatal(err)
	}
	type conv struct{ name, from, to, lib string }
	var convs []conv
	for rows.Next() {
		var cv conv
		if err := rows.Scan(&cv.name, &cv.from, &cv.to, &cv.lib); err != nil {
			t.Fatal(err)
		}
		convs = append(convs, cv)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	libs := map[string]bool{}
	for _, cv := range convs {
		// ASCII is the same in every server encoding.
		var b []byte
		if err := c.QueryRow(ctx, "SELECT convert('a'::bytea, $1, $2)", cv.from, cv.to).Scan(&b); err != nil || string(b) != "a" {
			t.Errorf("%s (%s to %s, %s): %q %v", cv.name, cv.from, cv.to, cv.lib, b, err)
		}
		libs[cv.lib] = true
	}
	t.Logf("%d default conversions in %d libraries", len(convs), len(libs))
	if len(libs) != 24 {
		t.Errorf("default conversions use %d libraries, want the 24 conversion_procs", len(libs))
	}
}

// TestLongCLoopsYield: C loops that never reach CHECK_FOR_INTERRUPTS poll
// the host themselves (patches 0006 and 0007), so a backend inside one does
// not hold up Go's stop-the-world. Without the patches each statement held
// it for much of its run (mul_var 165 to 180 ms, div_var 41 ms, sqrt through
// div_var_fast 26 ms); with them, under 1 ms. Each statement must run for
// at least minRun, so that the sampler sees it computing: a statement that
// finished at once would pass vacuously.
func TestLongCLoopsYield(t *testing.T) {
	if scaled(1) > 1 {
		t.Skip("PGMEM_TIME_SCALE > 1: the stop-the-world bound is the point of this test, so it is not scaled")
	}
	const (
		bound  = 15 * time.Millisecond // longest stop-the-world allowed
		minRun = 2 * bound             // shortest statement run that counts
	)
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	ctx := testCtx(t)
	exec(t, ctx, c, "CREATE TABLE big(v text)")
	for _, q := range []string{
		// TOAST compresses the 6.9 MB value with pglz (0006); checked below.
		"INSERT INTO big SELECT string_agg(i::text, ',') FROM generate_series(1, 1000000) i",
		// mul_var and div_var on 60,000- and 32,000-digit operands (0007).
		"SELECT length((repeat('9', 60000)::numeric * repeat('7', 60000)::numeric)::text)",
		"SELECT length(div(repeat('9', 64000)::numeric, repeat('7', 32000)::numeric)::text)",
		// sqrt of a 131,068-digit numeric divides through div_mod_var,
		// whose estimate is div_var_fast (0007). The digits vary: a run of
		// 9s gives zero quotient digits, which skip the inner loop.
		"SELECT length(sqrt(string_agg(lpad((i * 7919 % 1000003)::text, 7, '0'), '')::numeric)::text) FROM generate_series(1, 18724) i",
	} {
		var stop atomic.Bool
		var worst atomic.Int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			var ms runtime.MemStats
			for !stop.Load() {
				t0 := time.Now()
				runtime.ReadMemStats(&ms) // stops the world
				worst.Store(max(worst.Load(), int64(time.Since(t0))))
				time.Sleep(time.Millisecond)
			}
		}()
		halt := func() { stop.Store(true); <-done }
		t.Cleanup(halt) // a failing exec still stops the sampler
		t0 := time.Now()
		exec(t, ctx, c, q)
		run := time.Since(t0)
		halt()
		w := time.Duration(worst.Load())
		t.Logf("%s: ran %v; longest stop-the-world %v", q, run.Round(time.Millisecond), w)
		if run < minRun {
			t.Errorf("%s ran %v, want at least %v: too short to test", q, run, minRun)
		}
		if w > bound {
			t.Errorf("%s held up stop-the-world for %v, want at most %v", q, w, bound)
		}
	}
	var method string
	if err := c.QueryRow(ctx, "SELECT pg_column_compression(v) FROM big").Scan(&method); err != nil || method != "pglz" {
		t.Errorf("big.v compression %q, %v; want pglz (else the insert did not test 0006)", method, err)
	}
}

// The planner gives the same plan on every CPU. wazero v1.12.0's amd64
// backend miscompiled a float select in initial_cost_nestloop (wazero
// #2534), so Memoize costing read a stale register and amd64 chose a
// Nested Loop over Memoize (cost=0.29..28.83) where arm64 and the
// interpreter choose this Hash Join. The query is initdb's setup_privileges
// query, which fills pg_init_privs (src/bin/initdb/initdb.c, about line
// 1815). If a new cluster image changes the plan, check it against the
// plan from wazero's interpreter (wasmrt.Options.Interpreter).
func TestNestloopCostingSameOnEveryCPU(t *testing.T) {
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	ctx := testCtx(t)
	var plan string
	err := c.QueryRow(ctx, `EXPLAIN SELECT oid, pg_attribute.attnum, pg_attribute.attacl
		FROM pg_class JOIN pg_attribute ON (pg_class.oid = pg_attribute.attrelid)
		WHERE pg_attribute.attacl IS NOT NULL AND relkind IN ('r', 'v', 'm', 'S')`).Scan(&plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan, "Hash Join") {
		t.Fatalf("plan %q, want a Hash Join", plan)
	}
}
