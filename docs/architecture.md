# Architecture

pgmem runs PostgreSQL 17.11, with eight small patches, as WebAssembly inside a Go process. This page explains how that works and where the limits are. The [README](../README.md) covers installation and use.

## Overview

```
pgx --Dial--> in-memory conn --> kernel Unix socket --> postmaster (wasm instance)
                                                          | spawn
                                                          v
                                                  backend (wasm instance) <-- shared memory window
                                                          |
                                                        memfs (shared data directory)
```

`postgres.wasm` targets `wasm32-wasip1-threads` and is built with wasi-sdk 34. Stock [wazero](https://github.com/tetratelabs/wazero) compiles it once to native code. Every Postgres process is its own wazero instance of that compiled module, running in its own goroutine. That covers the postmaster, each backend, the checkpointer, the background writer and the WAL writer.

A small Go kernel sits between those instances. It provides the parts of a Unix system that Postgres needs and WebAssembly lacks.

## How Postgres runs

### Processes and EXEC_BACKEND

Postgres is built with `EXEC_BACKEND`. That is Postgres's own way to start each child as a fresh process, which its Windows port uses. WebAssembly has no `fork`, so this mode is the natural fit.

When the postmaster starts a child, patch 0003 replaces fork and exec with one call, `pgmem_spawn`. The kernel creates a new instance and runs `postgres --forkchild=... <param file>` in a new goroutine. The child inherits every file descriptor that is not marked close-on-exec, at the same number. Postgres relies on that to hand the client socket and its pipes to the child. `exit` and `waitpid` go through the kernel's process table, and the parent gets `SIGCHLD`.

### The kernel

The kernel lives in `internal/kernel`. It provides these pieces:

- **Processes.** A process table with spawn, exit, wait and process groups.
- **Signals.** Each process keeps pending, blocked and ignored masks in its own linear memory. A sender sets a pending bit and wakes the target. Every blocking host call (poll, read, write, accept, wait, semaphore wait, sleep) returns `EINTR` when a signal is deliverable. The host then calls the guest export `pgmem_signal_dispatch`, which runs the handlers. A backend busy in a loop sees signals through `CHECK_FOR_INTERRUPTS` (patch 0004). Timers deliver `SIGALRM`, so `statement_timeout` and `lock_timeout` work.
- **Shared memory.** The compat layer turns `shmget`, `shmat`, `shmdt` and `shmctl` into host calls. The host keeps one OS shared-memory object per segment, `memfd_create` on Linux and `shm_open` on macOS. It maps that object at the same guest address in every process, `0x2000000` (32 MiB). Postgres's own `sysv_shmem.c` stays unpatched. WebAssembly atomics compile to native atomic instructions, so spinlocks work across instances.
- **Semaphores.** Postgres's `posix_sema.c` stays unpatched. The compat layer's `sem_*` calls block in Go, keyed by where the `sem_t` sits in shared memory.
- **Unix sockets.** Stream sockets live in the kernel, with 256 KiB of buffer in each direction. The postmaster listens on `/tmp/.s.PGSQL.5432`. `Server.Dial` connects to it and returns a `net.Conn`. There is no TCP.
- **Files.** pgmem implements all of `wasi_snapshot_preview1` itself over memfs, an in-memory file tree that every instance shares. Each process has its own fd table, which the kernel can read and fill at spawn. wazero's built-in WASI cannot do either, and it cannot wait on pipes in `poll_oneoff`.

The guest imports `wasi_snapshot_preview1` and pgmem's own `pgm` module, which has 32 functions. The module exports an ABI version, and Go refuses to start a module with the wrong one.

### The compat layer

`postgres/compat` is a C library linked into `postgres.wasm`. It fills in the POSIX calls wasi-libc lacks, such as `sigaction`, `kill`, sockets, `popen`, `setitimer`, SysV shared memory and semaphores. Most of them become `pgm` host calls. `postgres/overlay` adds a `wasi` platform template to the Postgres source tree.

`setjmp` and `longjmp`, which `PG_TRY` and error handling use, compile to WebAssembly exception handling. Stock wazero supports it.

### Extensions

`plpgsql`, `btree_gist` and `dict_snowball` are linked statically. So are the 24 encoding conversion libraries and the regression test libraries. The compat layer's `dlopen` and `dlsym` resolve names from a generated table. Empty placeholder `.so` files satisfy Postgres's existence check, so its loader stays unpatched.

### Preemption

A WebAssembly loop with no host calls never reaches a Go safe point. That would block Go's stop-the-world garbage collection for the whole process. wazero can insert a check on every loop back-edge, but that made a CPU-bound query 3.1 times slower in our benchmarks.

pgmem leaves those checks off. Instead, every 4096th `CHECK_FOR_INTERRUPTS` makes a host call that returns at once (patch 0004). Patches 0006 and 0007 add the same call to two long C loops that never check for interrupts. With the yield, `runtime.GC()` during a CPU-bound query took about 2 ms, against 1.6 s without it.

## The patches

The patches in `postgres/patches` apply to the PostgreSQL 17.11 source. Every change sits under `#ifdef __wasi__` or compiles to the stock code without it.

| Patch | What it does |
|---|---|
| 0001 | Skips the data directory ownership check in `miscinit.c`, because WASI has no file ownership. |
| 0002 | Stops `collationcmds.c` from running `locale -a`, because WASI has no such program and wasi-libc knows only C and C.UTF-8. |
| 0003 | Starts EXEC_BACKEND children with `pgmem_spawn` instead of fork and exec. It also reads the backend parameter file at its real size, without the struct's trailing padding. |
| 0004 | Makes `CHECK_FOR_INTERRUPTS` check the process's pending signals, and yield to the host on every 4096th call. |
| 0005 | Marks the two values `PG_TRY` saves as `volatile`. This worked around wazero v1.12.0, whose catch handlers saw stale locals. The pinned wazero fixes that, and the patch stays because it is harmless. |
| 0006 | Yields to the host in the main loop of `pglz_compress`. |
| 0007 | Yields to the host in `mul_var`, `div_var` and `div_var_fast` in `numeric.c`, once per 64 digits, outside the inner loops. |
| 0008 | Pads every stack depth check with 256 bytes of shadow stack, and caps `max_stack_depth` at 512kB. See "Stack depth" below. |

## Memory

### Linear memory

`internal/vmem` backs each instance's linear memory. It reserves the 4 GiB maximum as address space up front and makes pages usable 1 MiB at a time as the memory grows. Pages become resident only when touched. Each process maps the shared window at 32 MiB, so the shared segment and the heap together must stay below 4 GiB.

The gap between a process's heap end and the window, about 21 MiB, counts as committed address space but is never touched. So measure resident memory, not committed bytes. On Linux use PSS, because RSS counts the shared segment once per process.

### Server settings

`make cluster` bakes these settings into the cluster image:

- `fsync=off`, `synchronous_commit=off`, `full_page_writes=off` and `autovacuum=off`;
- `shared_buffers=32MB`, `timezone=UTC` and `max_connections=200`;
- `dynamic_shared_memory_type=sysv`, `shared_memory_type=sysv` and `min_dynamic_shared_memory=32MB`;
- `max_worker_processes=2`, `max_parallel_workers=0`, `max_logical_replication_workers=0`, `wal_level=minimal` and `max_wal_senders=0`;
- `max_wal_size=64MB`, `min_wal_size=32MB` and `temp_file_limit=1GB`, because WAL and temporary files live in RAM;
- `listen_addresses=''` and `unix_socket_directories='/tmp'`.

initdb runs with 1 MiB WAL segments, which takes about 11 MB off every server. `pg_hba.conf` allows Unix-socket connections only, with trust authentication.

### Measured footprint

On an Apple M5 Pro with macOS, after `runtime.GC()`, a test process with one idle server was 181 MiB resident. Each idle connection adds a backend of about 5 MiB, so 20 connections came to 283 MiB. Go's heap was 48 to 67 MiB of that. Most of the rest is linear memory, the shared segment and compiled code. The engine's decompressed files, about 42 MB, stay in memory once the first server on that engine has started.

### Stack depth

Postgres's depth checks measure the shadow stack in linear memory. wazero keeps most locals on its own engine stack, which Postgres cannot see. Some recursions, such as the JSON parser's, grow the engine stack while the shadow stack hardly moves. Without a fix, a deeply nested JSON value overflowed the engine stack and crashed the backend.

Patch 0008 makes every depth check reserve 256 bytes of shadow stack in its caller's frame. So each level of a recursion grows the shadow stack, and `max_stack_depth` bounds it. The cap is 512kB, against a native default of 2MB. In measurements that allows JSON nesting to about 2000 levels, nested expressions to about 690, and plpgsql recursion to 253 levels. Deeper input fails with `ERROR 54001` and the session lives on.

Deep recursion also costs memory. wazero copies the engine stack for each active `PG_TRY`, and plpgsql holds two per level. So memory grows with the square of the depth, about 0.5 GB for a plpgsql recursion at the 253-level limit. A backend keeps its grown stack until an error returns to its main loop or the connection closes. Recycle pooled connections, or close a connection after a deliberately deep recursion.

## Engines and reproducible builds

`make wasm` builds Postgres in a pinned container. The image is Ubuntu 24.04, pinned by digest, with packages from a fixed `snapshot.ubuntu.com` date. The Postgres tarball and the wasi-sdk release are sha256-verified. The build copies the overlay, applies the patches, builds the compat library and compiles with `-O2`. It writes `postgres.wasm`, `initdb.wasm` and `share.tar`. `make repro` builds twice from scratch and checks that the outputs are byte-identical.

A native initdb cannot make the cluster image. wasm32 has a 4-byte `Datum` and passes `float8` by reference, and `pg_control` records both. So `make cluster` runs `initdb.wasm` inside pgmem. It runs deterministically. The clock starts at 2026-01-01 and moves 1 µs per read, each process gets a seeded random stream, timers never fire, and guests run on wazero's interpreter. Two runs give the same `cluster.tar` on every platform we tested.

`make assets` compresses `postgres.wasm`, `share.tar` and `cluster.tar` with zstd. The encoder settings give the same bytes on every run. The files total about 5.8 MB.

They don't live in this repository. The [pgmem-engine](https://github.com/feverenergy/pgmem-engine) repository has one Go module per Postgres major version, so far only `github.com/feverenergy/pgmem-engine/pg17`, tagged `pg17/v0.<Postgres minor>.<rebuild>`. The module embeds five files with `//go:embed`: the three archives, `SHA256SUMS` and `INPUTS.SHA256SUMS`. It exports them as `FS`, with the Postgres release as `Version`. `go get` fetches it like any other module, so users need no toolchain and tests need no network once the module cache has it. Keeping the binaries out of pgmem keeps pgmem's own history small, and a future `pg18` can ship as a module of its own.

`make assets` writes the engine files into a checkout of that repository, `ENGINE_DIR` (by default `../pgmem-engine/pg17`). Next to the archives it writes:

- `SHA256SUMS`, the sha256 of each compressed file. The engine module's tests check the embedded files against it.
- `INPUTS.SHA256SUMS`, the sha256 of each decompressed file. pgmem's `TestDefaultEngineInputSums` checks the files it decompresses against it.
- `PROVENANCE`, the build inputs from `build/versions.env` (the PostgreSQL, wasi-sdk and Ubuntu snapshot pins, and the Bison, flex and Go versions in the toolchain image), the sha256 of each patch, and one sha256 over the sources the build reads: `build/*.sh`, the `Containerfile`, `objs.mk`, `versions.env`, the compat layer's `include` and `src`, the overlay and the patches. `PROVENANCE` spells out how that sha256 is made, so anyone can check a pgmem commit against it. It says which sources the files came from without a clone of pgmem at the right commit.

The drift check is `make assets-check`. It rebuilds the files from the working tree into `build/out/assets-check` and compares them with `ENGINE_DIR` as it is on disk. It fails and names the file when the decompressed contents differ (the sources changed, so run `make assets`), when only the compression differs (a new zstd encoder, so `make assets` re-encodes), or when `INPUTS.SHA256SUMS` or `PROVENANCE` differ from what `make assets` would write. With `PGMEM_BUILD_DIR` set to a local build, `TestDefaultEngineMatchesBuild` makes the same comparison for the decompressed files from Go.

`pgmem.Engine` pairs an `FS` with its `Version`, and `pgmem.Postgres17` is the default. pgmem decompresses each of an engine's files on first use and keeps it for the life of the process, under the engine's `Version`. Two engines with one `Version` and different `FS` values are an error when pgmem can compare the two, because they would share those files. The build ID that names pgtest's cached template images hashes the `Version` and the decompressed files, so each engine gets its own images and its own pgtest servers.

`PGMEM_BUILD_DIR` points pgmem at a local build directory instead of any engine.

wazero compiles `postgres.wasm` on first use, which takes about 1.2 s. pgmem caches the result in `os.UserCacheDir()/pgmem/compiled/v2`, keyed by the module hash, wazero version, platform and preemption setting. Later starts take about 74 ms. Nothing removes entries for old wazero versions, so delete the directory after an upgrade if space matters.

## pgtest

Package `pgtest` gives each Go test its own database.

The first `pgtest.Pool` call for a `Template.Key` builds a template. It starts a server, creates database `tmpl`, runs `Migrate`, and installs pgtest's schema `_pgtest`. Then it shuts the server down cleanly and keeps the image. The image goes to `os.UserCacheDir()/pgmem/snapshots`, under a file lock, so parallel test binaries build it once. Later binaries boot from the cached file and skip `Migrate`. The file name hashes the Postgres build ID, pgtest's schema and the `Key`. So a new build or a new `Key` means a new template.

One server per test binary boots from that image. Each test gets database `db_<n>`, made with `CREATE DATABASE ... TEMPLATE tmpl STRATEGY FILE_COPY`, and a `pgxpool.Pool` of up to 20 connections. Cleanup drops the database when the test ends. If the test failed, cleanup first logs the database's server log lines with `t.Log`.

With `PGTEST_RECYCLE=1`, cleanup resets a passing test's database instead and keeps it for a later test. The reset truncates what the test wrote, reloads the template's rows and moves sequences back. An event trigger marks a database dirty on DDL, and a dirty database is dropped, not reused. Fresh copies are the default because they are usually cheaper. In `BenchmarkPerTestDatabase` and `BenchmarkPerTestDatabaseParallel`, one test at a time, a test's database cost 6.1 ms fresh and 9.1 ms recycled on 50 tables, and 9.6 ms against 20.3 ms on 200. Recycling can win when many tests run at once, with 3.2 ms against 5.3 ms at 8 parallel tests on 50 tables.

The server's `max_connections` follows `-test.parallel`. It is `max(500, 20*N + 20)`, capped at 2620. Pools close connections after 1 s idle.

A guard samples the test process's resident memory every 500 ms. Past `PGTEST_MAX_RSS_MB` (4096 by default) it prints the end of each server's log and exits with code 3.

## Limitations

- **Stack depth.** `max_stack_depth` is capped at 512kB. plpgsql recursion stops at about 250 levels.
- **Parallel query.** It is off (`max_parallel_workers=0`).
- **Extensions.** Only `plpgsql`, `btree_gist` and `dict_snowball` are available. Logical replication is not linked.
- **Locales.** C and C.UTF-8 only.
- **Network.** Clients connect through `Server.Dial`. The server has no TCP listener, and `cmd/pgmem-serve` adds one on loopback for native tools.
- **Security.** Authentication is trust and every client is superuser. pgmem is for tests, not production.
- **Long C loops.** A C loop that never reaches `CHECK_FOR_INTERRUPTS` can still delay Go's garbage collector until its next host call. Candidates are regex compilation and large in-memory sorts.
- **Windows.** It is not supported.
- **Regression suite.** 222 of PostgreSQL's 225 regression tests pass. `select_parallel` differs because parallel query is off. `subscription` and `object_address` differ because logical replication is not linked.

### The wazero pin

pgmem uses stock wazero, with no fork or local patch. It pins an untagged commit of upstream `main`, `v1.12.1-0.20260829084255-f4779551afb4`, for two reasons:

- wazero v1.12.0's amd64 backend miscompiles some floating-point `select` instructions (wazero #2534, fixed by #2535). In `postgres.wasm` that changed query plans on amd64.
- The next commit on `main`, from #2538, rewrote `memory.fill`. On amd64 CPUs without AVX2 it can leave nonzero bytes in memory that should be zeroed.

The pinned commit has the first fix and predates the second change. `TestZeroFillClears` and `TestXmmCMovFloatSelect` guard both. pgmem will move to a tagged release once one has both fixes. A module that depends on pgmem inherits this pseudo-version.

wazero issue #2522 reports a use-after-free in exception handling. We checked `postgres.wasm`, and its exception code does not reach that path.
