# pgmem

pgmem runs real PostgreSQL 17.11 inside your Go test process. Postgres is compiled to WebAssembly and runs on [wazero](https://github.com/tetratelabs/wazero), with a small Go kernel for processes, signals, shared memory and sockets. `pgmem.Start` gives you a server, and package `pgtest` gives each test its own migrated database.

## Why

- **Real Postgres 17.** It is the PostgreSQL server, with [eight small patches](docs/architecture.md#the-patches) (in [`postgres/patches/`](postgres/patches/)). Every connection is a real backend, so transactions, locks, `LISTEN`/`NOTIFY`, plpgsql and the planner behave as they do on a native server. 222 of PostgreSQL's 225 regression tests pass.
- **Nothing to install.** The server comes as a Go module, which `go get` fetches with pgmem. You need no Postgres binaries, no Docker, no open ports and no temporary directories.
- **Fast.** A server starts in about 74 ms, and a test gets a fresh database in a few milliseconds.

## Install

```sh
go get github.com/feverenergy/pgmem
```

pgmem needs Go 1.27 or later.

## Quick start

### A database per test with pgtest

`pgtest.Pool` returns a `pgxpool.Pool` connected to a new database, copied from a migrated template. pgtest builds the template once and caches it on disk.

```go
import (
	"context"
	"testing"

	"github.com/feverenergy/pgmem/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

var tpl = pgtest.Template{
	// Change Key whenever the schema changes, for example to a hash
	// of the migration files.
	Key: "schema-v1",
	Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pool.Exec(ctx, `
			CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric NOT NULL);
			CREATE INDEX ON orders (total);
		`)
		return err
	},
}

func TestOrders(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, tpl) // a database of its own; cleanup drops it
	if _, err := pool.Exec(context.Background(), "INSERT INTO orders (total) VALUES (42)"); err != nil {
		t.Fatal(err)
	}
}
```

With [goose](https://github.com/pressly/goose), run the migrations through a `database/sql` handle on the pool:

```go
Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	db.SetMaxIdleConns(0) // Migrate must return every connection to the pool
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
},
```

### A server with pgmem.Start

```go
import (
	"context"

	"github.com/feverenergy/pgmem"
	"github.com/jackc/pgx/v5"
)

func run(ctx context.Context) error {
	srv, err := pgmem.Start(ctx, pgmem.Options{})
	if err != nil {
		return err
	}
	defer srv.Close()

	cfg, err := pgx.ParseConfig("host=/tmp user=postgres sslmode=disable")
	if err != nil {
		return err
	}
	cfg.DialFunc = srv.Dial // every connection goes through the in-process server
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}
```

`Server.Snapshot` returns an image of a running server, and `Server.Shutdown` returns one after a clean shutdown. `Options.Image` boots a new server from either. Package `pgmem` does not depend on pgx.

To use `psql` or another native client, run one server on a Unix socket:

```sh
go run github.com/feverenergy/pgmem/cmd/pgmem-serve
```

It prints `ready <socket path>`. `-tcp 127.0.0.1:PORT` adds a loopback TCP listener, which gives every local user superuser access. Run it with `-h` for the other flags.

## How it works

Every Postgres process is its own wazero instance of one compiled `postgres.wasm`, in its own goroutine. Postgres is built with `EXEC_BACKEND`, so it starts each child as a new process instead of forking. A Go kernel gives those instances processes, signals, SysV shared memory, semaphores, Unix sockets and a shared in-memory file system.

[docs/architecture.md](docs/architecture.md) explains the design, the patches and the limits.

## Engines

The compiled server is not in this module. It lives in an engine module, [`github.com/feverenergy/pgmem-engine/pg17`](https://github.com/feverenergy/pgmem-engine), as zstd-compressed files of about 5.8 MB in all. pgmem requires that module, so `go get` fetches it too, and Postgres 17 is the default. There is nothing to import.

To name the engine, set it in the options:

```go
srv, err := pgmem.Start(ctx, pgmem.Options{Engine: pgmem.Postgres17})

var tpl = pgtest.Template{Key: "schema-v1", Migrate: migrate, Engine: pgmem.Postgres17}
```

`pgmem.Engine` is a `Version` and an `FS`, and any module laid out like `pg17` works as one. One process can run several engines. Each needs its own `Version`, and pgtest gives each its own servers and cached images. Postgres 17 is the only engine so far.

## Performance

Measured on an Apple M5 Pro (18 cores, 48 GB), macOS 27.0, Go 1.27, with a warm compile cache.

| What | Time |
|---|---|
| `pgmem.Start` until ready | about 74 ms |
| pgx connect, `SELECT 1`, close | about 1.1 ms |
| `SELECT 1` on an open connection | about 14 µs |
| `count(*)` over 3 million rows from `generate_series` | about 173 ms |
| `Server.Snapshot` | about 10 ms idle, 17 to 48 ms while clients write |
| First `Start` on a machine, compiling `postgres.wasm` | about 1.2 s, once |

pgtest's costs depend on the template. These come from `BenchmarkPerTestDatabase` and `BenchmarkPerTestDatabaseParallel` in `pgtest/pgtest_test.go`:

- Building a template runs `Migrate` and saves the image, so it costs about as much as your migrations. Later test binaries load the cached image instead.
- On a 50-table template, `CREATE DATABASE` took 2.3 ms, and a whole test's database, created, used and dropped, 6.1 ms. Recycled with `PGTEST_RECYCLE=1`, it took 9.1 ms.
- On 200 tables, one test at a time, a test's database took 9.6 ms fresh and 20.3 ms recycled.
- Run in parallel, fresh databases stop getting cheaper at about 5.3 ms each on 50 tables and 9.6 ms on 200, because each create waits for a checkpoint. Recycling wins there on the small template, at 3.2 ms per test with 8 parallel tests.

Fresh databases are the default, because one test at a time they were cheaper than recycled ones on both template sizes.

Each cached template image is a whole cluster, tens of MB. Whenever pgtest writes a new image, it deletes the images not used for 30 days.

Memory, as resident memory of the whole test process:

- One idle server is about 181 MiB.
- Each open connection is a backend of about 5 MiB, so 20 connections come to about 283 MiB.
- A test process with a pgtest server and one connection was 370 to 400 MiB.

Every test binary that uses pgtest runs its own server. `go test -p N` bounds how many run at once, so choose `-p` for memory, not cores.

## Supported platforms

| OS | arm64 | amd64 |
|---|---|---|
| macOS | yes | untested |
| Linux | yes | yes |
| Windows | no | no |

CI runs gofmt, `go vet` and every test on GitHub's Linux amd64, Linux arm64 and macOS arm64 runners. pgmem builds for macOS amd64, but no tests run there, so it is untested. `make test-linux-amd64` runs the tests under qemu-user emulation, for checking amd64 from an arm64 machine.

## Environment variables

| Variable | Effect |
|---|---|
| `PGTEST_CACHE_DIR` | Where pgtest caches template images. It must be an absolute path. `off` builds them in memory on every run. The default is `os.UserCacheDir()/pgmem/snapshots`. |
| `PGTEST_RECYCLE` | `1` resets the database of a passing test and gives it to a later test, instead of dropping it. |
| `PGTEST_MAX_RSS_MB` | The test process's resident memory limit in MiB. Past it, pgtest prints each server's log tail and exits with code 3. The default is 4096, and `0` turns the guard off. |
| `PGMEM_BUILD_DIR` | An absolute path to a local build (`make wasm cluster`). `pgmem.Start` then loads `postgres.wasm`, `share.tar` and `cluster.tar` from it instead of the engine's files, whatever the engine. |

## Limitations

- `max_stack_depth` is capped at 512kB, so plpgsql recursion stops at about 250 levels.
- Deep recursion costs memory that grows with the square of the depth. A backend keeps that memory until a statement fails or its connection closes.
- Parallel query is off.
- The only extensions are `plpgsql`, `btree_gist` and `dict_snowball`. Logical replication is not available.
- Locales are C and C.UTF-8 only.
- Authentication is trust and every client is superuser. pgmem is for tests, not production.
- Windows is not supported.
- pgmem pins an untagged wazero commit, because v1.12.0 miscompiles some float code on amd64 and later `main` commits have a separate amd64 bug. Your module inherits that pin.

[docs/architecture.md](docs/architecture.md#limitations) has the details.

## Building from source

The engine module holds the built files, so you only need this to change Postgres, the patches or the build. You need Go 1.27 and Podman, or Docker with `CONTAINER=docker`. [CONTRIBUTING.md](CONTRIBUTING.md) has the workflow and the patch policy.

```sh
make toolchain         # build the pinned toolchain image for this CPU, once
make toolchain-check   # check that the image has the pinned tools for this CPU
make test              # go test ./..., against the engine checkout when there is one
make fixtures          # recompile the C test programs into the committed .wasm fixtures
make wasm              # build/out/postgres.wasm, initdb.wasm and share.tar
make cluster           # build/out/cluster.tar, made by initdb running inside pgmem
make assets            # wasm and cluster, compressed into $(ENGINE_DIR)
make assets-check      # rebuild the engine files from source and fail if those in $(ENGINE_DIR) differ
make check             # fixtures, wasm and cluster, then every test here and on Linux against that build
make repro             # two builds from scratch must match byte for byte
make repro-cross       # the amd64 toolchain must build the same bytes as this CPU's
make regress           # PostgreSQL's regression suite against pgmem, after make wasm cluster
make test-linux        # every test in the Linux toolchain container, against build/out
make test-linux-amd64  # make test-linux under amd64 emulation, with 10 times the time bounds
```

`make check` runs `make fixtures`, `make wasm` and `make cluster`, and fails if a `.wasm` fixture changed. Then it runs `go test ./...` on the host and in the Linux container, both with `PGMEM_REQUIRE_BUILD=1` and `PGMEM_BUILD_DIR` set to `build/out`. `make repro-cross` needs `make toolchain ARCH=amd64` first and is slow, because it runs emulated.

`make regress` also builds native `psql` and `pg_regress` on the host. That needs a C compiler, `make`, `bison`, `flex`, `perl`, `bzip2` and `tar`. On macOS `xcode-select --install` provides them. It compares the results with `postgres/regress-allowlist`.

Run the tests against a local build with:

```sh
PGMEM_REQUIRE_BUILD=1 PGMEM_BUILD_DIR=$PWD/build/out go test ./...
```

### Developing against a local engine

`make assets` and `make assets-check` expect a checkout of [pgmem-engine](https://github.com/feverenergy/pgmem-engine) next to this repo, and `ENGINE_DIR` defaults to `../pgmem-engine/pg17`. Set `ENGINE_DIR` to use another one.

When `$(ENGINE_DIR)/go.mod` exists, every Go command the Makefile runs uses that checkout instead of the engine version in `go.mod`. The Makefile writes a `go.work` to `build/out/go.work` that uses this module and the engine and replaces the required engine version with the checkout, and it exports `GOWORK` pointing there. For `make test-linux` it also mounts the checkout read-only at `/engine` in the container, with a matching `go.work` at `/gowork/go.work`. Both files are under the ignored `build/out`, and no `go.work` is committed. Without the checkout, Go fetches the engine from the module proxy, as it does for any user. A `GOWORK` set in your environment wins. `GOWORK=off make check` tests against the published engine.

Plain `go` commands outside the Makefile don't see the checkout. To run them against it, point `GOWORK` at the Makefile's file, for example `GOWORK=$PWD/build/out/go.work go test ./...`, after any `make` run has written it.

## License

pgmem is licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Fever Energy AS.

[LICENSES/README.md](LICENSES/README.md) lists the third-party code in this repository and the Go modules it uses, and `LICENSES/` holds their texts. The compiled server in the engine module includes PostgreSQL, wasi-libc and other components under their own licenses, listed in that repository's `pg17/LICENSES/`.
