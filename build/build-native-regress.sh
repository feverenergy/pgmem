#!/usr/bin/env bash
# Builds native psql and pg_regress for make regress, on the host, from the
# same sha256-pinned PostgreSQL tarball as the wasm build. Usage:
#
#	build/build-native-regress.sh [OUT]   (default build/out)
#
# Everything goes under OUT/native-<goos>-<goarch>/: the source tree, with
# pg_regress and the regression suite in src/test/regress, and an install
# prefix holding psql and libpq. Nothing is installed anywhere else. The
# build is skipped when a stamp records the same tarball hash and the same
# version of this script.
#
# macOS uses Xcode's clang; Linux uses the system cc ($CC overrides both).
# The host needs that compiler, make, bison, flex, perl, bzip2 and tar; the
# script checks for them first.
# The tarball comes from $PG_TARBALL if set, else from the toolchain image's
# /opt/src (podman, or $CONTAINER); its sha256 is checked either way.
# These binaries only drive the tests, so they need not be reproducible:
# their output is compared with the tarball's expected files.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=versions.env
. "$REPO/build/versions.env"
OUT=$(mkdir -p "${1:-$REPO/build/out}" && cd "${1:-$REPO/build/out}" && pwd)
GOOS=$(go env GOOS)
GOARCH=$(go env GOARCH)
N=$OUT/native-$GOOS-$GOARCH
SRC=$N/postgresql-$PG_VERSION
TARBALL=$N/postgresql-$PG_VERSION.tar.bz2

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

case "$GOOS" in
darwin)
	CC=${CC:-/usr/bin/clang} # the shim that picks the SDK
	JOBS=$(sysctl -n hw.ncpu)
	;;
*)
	CC=${CC:-cc}
	JOBS=$(nproc)
	;;
esac

# The stamp names the compiler too: changing $CC rebuilds.
STAMP="$PG_SHA256 $(sha256 "$0") CC=$CC"
if [ -f "$N/stamp" ] && [ "$(cat "$N/stamp")" = "$STAMP" ] &&
	[ -x "$N/install/bin/psql" ] && [ -x "$SRC/src/test/regress/pg_regress" ]; then
	echo "native psql and pg_regress $PG_VERSION are up to date in $N"
	exit 0
fi

# Host tools the build needs. PostgreSQL 17 tarballs no longer ship the
# generated parsers and scanners, so bison and flex are required; perl
# generates headers; tar -j needs bzip2.
missing=
for tool in "${CC%% *}" make bison flex perl bzip2 tar; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
	echo "build-native-regress.sh: missing host tools:$missing" >&2
	echo "  needs a C compiler (\$CC, default $([ "$GOOS" = darwin ] && echo /usr/bin/clang || echo cc)), make, bison, flex, perl, bzip2 and tar" >&2
	echo "  (macOS: xcode-select --install; Debian/Ubuntu: apt install build-essential bison flex perl bzip2)" >&2
	exit 1
fi

mkdir -p "$N"
rm -rf "$SRC" "$N/install" "$N/stamp"
if [ ! -f "$TARBALL" ] || [ "$(sha256 "$TARBALL")" != "$PG_SHA256" ]; then
	if [ -n "${PG_TARBALL:-}" ]; then
		cp "$PG_TARBALL" "$TARBALL.tmp"
	else
		ARCH=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
		echo "extracting postgresql-$PG_VERSION.tar.bz2 from pgmem-toolchain:$ARCH"
		"${CONTAINER:-podman}" run --rm --platform "linux/$ARCH" "pgmem-toolchain:$ARCH" \
			cat "/opt/src/postgresql-$PG_VERSION.tar.bz2" >"$TARBALL.tmp"
	fi
	got=$(sha256 "$TARBALL.tmp")
	if [ "$got" != "$PG_SHA256" ]; then
		echo "postgresql-$PG_VERSION.tar.bz2: sha256 $got, want $PG_SHA256" >&2
		rm -f "$TARBALL.tmp"
		exit 1
	fi
	mv "$TARBALL.tmp" "$TARBALL"
fi
tar -xjf "$TARBALL" -C "$N"

echo "building native psql and pg_regress $PG_VERSION with $CC (logs in $N)"
cd "$SRC"
./configure --prefix="$N/install" CC="$CC" --without-icu --without-readline --without-zlib \
	--without-openssl --without-zstd --without-lz4 --without-libxml --without-llvm \
	>"$N/configure.log" 2>&1 || { tail -20 "$N/configure.log" >&2; exit 1; }
{
	make -C src/backend generated-headers
	make -j"$JOBS" -C src/port
	make -j"$JOBS" -C src/common
	make -j"$JOBS" -C src/interfaces/libpq
	make -j"$JOBS" -C src/fe_utils
	make -j"$JOBS" -C src/bin/psql
	# Only the driver: "all" would also link regress, refint and autoinc
	# against a native postgres binary (on macOS with -bundle_loader), which
	# is never built. pgmem links those libraries into postgres.wasm.
	make -C src/test/regress pg_regress
	# psql links libpq from the install prefix.
	make -C src/interfaces/libpq install
	make -C src/bin/psql install
} >"$N/make.log" 2>&1 || { tail -30 "$N/make.log" >&2; exit 1; }
"$N/install/bin/psql" --version
echo "$STAMP" >"$N/stamp"
