# Licenses

pgmem's own code is licensed under the Apache License 2.0, in `LICENSE` at the top of the repository, with the notice in `NOTICE`. That covers the Go code, the Postgres patches, the overlay and the compat layer. This directory holds the license texts of the third-party code in this repository and of every Go module that pgmem, pgtest or the tests import. Texts are copied verbatim from the sources in the last column of each table.

The compiled PostgreSQL server is not in this repository. It lives in the engine module [`github.com/feverenergy/pgmem-engine/pg17`](https://github.com/feverenergy/pgmem-engine), and the licenses of the code inside it (PostgreSQL, its bundled code, Snowball, wasi-libc, compiler-rt and tzdata) are in that repository's `pg17/LICENSES/`, with an index in `pg17/LICENSES/README.md`.

## PostgreSQL

| Component | Version | License | Where used | License file (source of the text) |
|---|---|---|---|---|
| PostgreSQL | 17.11 | PostgreSQL License | `postgres/patches/` change PostgreSQL's source, and `postgres/overlay/` adds files derived from it | `postgresql.txt` (`COPYRIGHT` in `postgresql-17.11.tar.bz2`) |

## The test fixtures

The `.wasm` files under `internal/*/testdata/` and `postgres/compat/testdata/` are small C test programs that `make fixtures` compiles with wasi-sdk 34. Most of them link wasi-libc and the compiler-rt builtins, so the repository carries those licenses.

| Component | Version | License | Where used | License file (source of the text) |
|---|---|---|---|---|
| wasi-libc | wasi-sdk 34 (wasi-libc `2e6fb9d8ee0c`) | Apache-2.0 WITH LLVM-exception, Apache-2.0 and MIT (multi-licensed) | libc and the WASI bottom half linked into the fixtures | `wasi-libc-NOTICE.txt` (`LICENSE`), `wasi-libc-Apache-2.0-LLVM-exception.txt` (`LICENSE-APACHE-LLVM`), `wasi-libc-Apache-2.0.txt` (`LICENSE-APACHE`), `wasi-libc-MIT.txt` (`LICENSE-MIT`), all from wasi-libc at `2e6fb9d8ee0c` |
| musl libc (inside wasi-libc `libc-top-half`) | as vendored by wasi-libc | MIT, with other permissive notices listed in the file | string, stdio, stdlib and similar functions | `musl.txt` (`libc-top-half/musl/COPYRIGHT`, wasi-libc `2e6fb9d8ee0c`) |
| cloudlibc (inside wasi-libc `libc-bottom-half`) | as vendored by wasi-libc | BSD-2-Clause | the WASI syscall wrappers | `cloudlibc.txt` (`libc-bottom-half/cloudlibc/LICENSE`, wasi-libc `2e6fb9d8ee0c`) |
| dlmalloc (inside wasi-libc) | 2.8.6 | CC0 (public domain dedication) | `malloc`, `free` | `dlmalloc.txt` (the notice at the top of `dlmalloc/src/malloc.c`, wasi-libc `2e6fb9d8ee0c`) |
| compiler-rt builtins (wasi-sdk 34, LLVM 23.1.0) | 23.1.0 | Apache-2.0 WITH LLVM-exception (the file also lists the older dual license of some files) | integer helpers such as `__multi3` | `compiler-rt.txt` (`compiler-rt/LICENSE.TXT` at llvm-project commit `895aa2c896ad`, the LLVM wasi-sdk 34 was built from; identical to tag `llvmorg-23.1.0`) |

## Go dependencies of the library

| Component | Version | License | Where used | License file (source of the text) |
|---|---|---|---|---|
| wazero | v1.12.1-0.20260829084255-f4779551afb4 (upstream `main`) | Apache-2.0 | The WebAssembly runtime | `wazero.txt` and `wazero-NOTICE.txt` (`LICENSE`, `NOTICE`, module cache) |
| klauspost/compress | v1.20.1 | BSD-3-Clause | zstd decoding of the engine files (`zstd`, `fse`, `huff0`), and `mkassets` encoding | `klauspost-compress.txt` (`LICENSE`, module cache) |
| klauspost/compress `internal/snapref` | in v1.20.1 | BSD-3-Clause (Snappy-Go Authors) | compiled into `zstd` | `klauspost-compress-snapref.txt` (`internal/snapref/LICENSE`) |
| cespare/xxhash, in klauspost/compress `zstd/internal/xxhash` | in v1.20.1 | MIT | compiled into `zstd` | `klauspost-compress-xxhash.txt` (`zstd/internal/xxhash/LICENSE.txt`) |
| golang.org/x/sys | v0.44.0 | BSD-3-Clause | `internal/vmem` (mmap, shared segments) | `golang-x-sys.txt` (`LICENSE`, module cache) |

klauspost/compress ships other sub-packages with their own licenses: `s2`, `gzhttp`, `snappy` and `lz4ref`. pgmem does not import them. `go list -deps` of `zstd` shows the root `github.com/klauspost/compress` package, `fse`, `huff0`, `internal/le`, `internal/snapref` and `zstd/internal/xxhash` on every GOARCH, plus `internal/cpuinfo` on amd64. All but `snapref` and `xxhash` are under the main `LICENSE`.

## Go dependencies of package `pgtest`

The public package `pgtest` returns a pgx `pgxpool.Pool`, so a module that imports `pgtest` also builds these Go modules. The root package `pgmem` does not import them.

| Component | Version | License | Where used | License file (source of the text) |
|---|---|---|---|---|
| jackc/pgx | v5.9.2 | MIT | `pgxpool`, the client pgtest returns | `pgx.txt` (`LICENSE`, module cache) |
| jackc/pgpassfile | v1.0.0 | MIT | used by pgx | `pgpassfile.txt` (`LICENSE`, module cache) |
| jackc/pgservicefile | v0.0.0-20240606120523-5a60cdf6a761 | MIT | used by pgx | `pgservicefile.txt` (`LICENSE`, module cache) |
| jackc/puddle | v2.2.2 | MIT | the pool under `pgxpool` | `puddle.txt` (`LICENSE`, module cache) |
| golang.org/x/sync | v0.20.0 | BSD-3-Clause | used by puddle | `golang-x-sync.txt` (`LICENSE`, module cache) |
| golang.org/x/text | v0.37.0 | BSD-3-Clause | used by pgx | `golang-x-text.txt` (`LICENSE`, module cache) |

## Test dependencies

Only `go test` in this repository builds this module:

| Component | Version | License | Where used | License file (source of the text) |
|---|---|---|---|---|
| golang.org/x/net | v0.54.0 | BSD-3-Clause | `nettest`, the `net.Conn` conformance test in `internal/wasi` | `golang-x-net.txt` (`LICENSE`, module cache) |

## Build-time only

wasi-sdk (clang, wasm-ld) and the Ubuntu packages in the toolchain image build the engine files and the fixtures. They are not part of either beyond the libraries above.
