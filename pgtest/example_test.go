package pgtest_test

import (
	"context"
	"testing"

	"github.com/feverenergy/pgmem/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tpl is the template that each test's database is copied from. Change
// Key whenever Migrate's result changes, for example to a hash of the
// migration files.
var tpl = pgtest.Template{
	Key: "orders-v1",
	Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pool.Exec(ctx, `
			CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric NOT NULL);
			CREATE INDEX ON orders (total);
		`)
		return err
	},
}

// In your own _test.go file, name this TestOrders, and go test runs it.
func testOrders(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, tpl) // a database of its own; cleanup drops it
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO orders (total) VALUES (42)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("orders has %d rows, want 1", n)
	}
}

// Give each test a database of its own, copied from a migrated template.
// The test function needs the *testing.T that go test passes it, so this
// example only names it.
func ExamplePool() {
	_ = testOrders
}
