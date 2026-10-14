# Contributing

Issues and pull requests are welcome. For a large change, open an issue first so we can agree on the approach.

## Build and test

You need Go 1.27. Most changes, such as to the kernel, pgtest or the Go API, need nothing else:

```sh
go vet ./...
go test ./...
```

The tests use the engine from the module cache. Each test binary that starts a server uses a few hundred MiB, so on a small machine lower `go test -p`.

Changes to Postgres, the patches, the compat layer, the overlay or the build need the toolchain container, which runs with Podman, or Docker with `CONTAINER=docker`:

```sh
make toolchain   # once per CPU
make check       # fixtures, wasm and cluster, then every test on the host and on Linux against that build
make regress     # PostgreSQL's regression suite
```

With Docker on a Linux host, add `RUN_FLAGS="--user $(id -u):$(id -g) -e HOME=/tmp"` to the targets that build in the container: `make fixtures`, `make wasm` and those that run them, such as `make check`, `make assets` and `make assets-check`. Then the files the build writes in `build/out` belong to you and not to root.

`make check` fails if a committed `.wasm` fixture changed. Commit the new fixtures with the C change that caused them.

`make regress` must report 222 of 225 tests passing. `postgres/regress-allowlist` lists the three known differences, each with a hash of its diff, and the check fails on any other failure, on a changed diff and on a listed test that now passes. If your change fixes a listed test, remove its line. If it changes a listed diff on purpose, update the hash and say why in the pull request.

The README lists every make target.

## Rebuilding an engine

The compiled server lives in the [pgmem-engine](https://github.com/feverenergy/pgmem-engine) repository, one Go module per Postgres major version. To rebuild it after a change here:

1. Check out pgmem-engine next to this repository, or set `ENGINE_DIR` to its `pg17` directory. The Makefile then builds and tests against that checkout (see "Developing against a local engine" in the README).
2. Run `make assets`. It builds `postgres.wasm` and the cluster image and writes the compressed files, `SHA256SUMS`, `INPUTS.SHA256SUMS` and `PROVENANCE` into the checkout.
3. Run `make check` and `make regress` against it, and `make assets-check`, which rebuilds from scratch and must report that the files match.
4. Commit the files in pgmem-engine and tag the module. A rebuild of Postgres 17.11 is `pg17/v0.11.1`, and the first build of 17.12 is `pg17/v0.12.0`. Then raise the requirement in pgmem's `go.mod` in the same pull request as the source change.

The build is reproducible. `make repro` checks that two builds from scratch give the same bytes, and `make repro-cross` that the amd64 toolchain gives the same bytes as the native one. A pull request that changes the engine files should leave both passing.

## Patch policy

pgmem changes PostgreSQL as little as it can. Most of the porting work lives outside the Postgres tree, in the Go kernel and in the compat layer (`postgres/compat`), which supplies the POSIX calls WASI lacks. A patch to Postgres itself is the last resort.

- Each patch in `postgres/patches/` does one thing and is numbered in the order it applies.
- Every change sits under `#ifdef __wasi__` or compiles to the stock code without it.
- A new or changed patch needs a row in the patch table in [docs/architecture.md](docs/architecture.md#the-patches) that says what it does and why the kernel or compat layer could not do it instead.
- The engine's `PROVENANCE` records the sha256 of every patch and of the build sources, the compat layer included. So a change to any of them means rebuilding the engine, and `make assets-check` fails until you do.

## Style

Run `gofmt`. Comments and docs say what the code does and why, in plain sentences. Match the surrounding code.

## License

By contributing, you agree that your contributions are licensed under the Apache License 2.0, as the rest of pgmem.
