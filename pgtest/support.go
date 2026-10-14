package pgtest

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem"
)

// supportSQL creates schema _pgtest: the seed and sequence tables, capture,
// reset and the DDL guard's trigger function, but not the event trigger.
//
//go:embed support.sql
var supportSQL string

// installSupport installs schema _pgtest in the template database after
// Migrate. It runs support.sql, captures the template's data, then creates
// the DDL guard's event trigger. The trigger comes last because capture
// creates tables, which would mark the template dirty. The trigger always
// fires, including under session_replication_role = replica. It all runs
// in one transaction, so a failure leaves no _pgtest schema behind.
//
// Then it zeroes the template's cumulative statistics. The reset then sees
// any write to _pgtest's tables as a count above zero, in the template and
// in its copies, which start with no statistics. The backend writes the
// install's own counts first, because pg_stat_force_next_flush makes it
// write them before it answers that statement.
func installSupport(ctx context.Context, conn *pgx.Conn) error {
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, step := range []struct{ name, sql string }{
			{"support.sql", supportSQL},
			{"capture", "SELECT _pgtest.capture()"},
			{"event trigger", "CREATE EVENT TRIGGER _pgtest_ddl ON ddl_command_start EXECUTE FUNCTION _pgtest.mark_dirty()"},
			// The default (origin) state is skipped under
			// session_replication_role = replica. reset runs under replica
			// too, but it does no DDL, and TRUNCATE fires no event trigger.
			{"enable event trigger", "ALTER EVENT TRIGGER _pgtest_ddl ENABLE ALWAYS"},
			{"state", "UPDATE _pgtest.state SET dirty = false"},
		} {
			if _, err := tx.Exec(ctx, step.sql); err != nil {
				return fmt.Errorf("%s: %w", step.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("pgtest: install support schema: %w", err)
	}
	for _, sql := range []string{"SELECT pg_stat_force_next_flush()", "SELECT pg_stat_reset()"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			return fmt.Errorf("pgtest: install support schema: %s: %w", sql, err)
		}
	}
	return nil
}

// resetDatabase restores the template's data in the database conn is
// connected to, which must be its only session. ok=false means the
// database must be dropped, not reused, and reason says why: "other
// sessions", "pgtest tables changed", "ddl", "database settings" or
// "large objects".
func resetDatabase(ctx context.Context, conn *pgx.Conn) (ok bool, reason string, err error) {
	if err := conn.QueryRow(ctx, "SELECT ok, reason FROM _pgtest.reset()").Scan(&ok, &reason); err != nil {
		return false, "", fmt.Errorf("pgtest: reset: %w", err)
	}
	return ok, reason, nil
}

// connString is the connection string for srv, without the database.
// Connections reach srv through its own Dial, so the host only names the
// Unix socket directory, and pgx does no lookup for it.
const connString = "host=/tmp user=postgres sslmode=disable"

// connConfig is the pgx configuration for database db on srv. Every dial,
// CancelRequests included, goes through srv.Dial.
func connConfig(srv *pgmem.Server, db string) *pgx.ConnConfig {
	cfg, err := pgx.ParseConfig(connString)
	if err != nil {
		panic(fmt.Sprintf("pgtest: %q: %v", connString, err)) // a constant string
	}
	cfg.Database = db
	cfg.DialFunc = srv.Dial
	return cfg
}

// poolConfig is the pgxpool configuration for database db on srv, with at
// most maxConns connections. The pool closes a connection that has been
// idle for a second, and checks every second. Each connection is a backend
// of its own, and a parent test keeps its pool while its parallel subtests
// wait for their turn.
func poolConfig(srv *pgmem.Server, db string, maxConns int32) *pgxpool.Config {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		panic(fmt.Sprintf("pgtest: %q: %v", connString, err)) // a constant string
	}
	cfg.ConnConfig.Database = db
	cfg.ConnConfig.DialFunc = srv.Dial
	cfg.MaxConns = maxConns
	cfg.MaxConnIdleTime = time.Second
	cfg.HealthCheckPeriod = time.Second
	return cfg
}
