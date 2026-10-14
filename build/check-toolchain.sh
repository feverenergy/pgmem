#!/usr/bin/env bash
# Verifies the toolchain image matches build/versions.env and the CPU the
# caller asked for (EXPECTED_ARCH, arm64 or amd64).
set -euo pipefail
export LC_ALL=C
source /repo/build/versions.env

fail() {
	echo "toolchain check failed: $*" >&2
	exit 1
}

case "$(uname -m)" in
	aarch64) arch=arm64 ;;
	x86_64) arch=amd64 ;;
	*) fail "unexpected CPU $(uname -m)" ;;
esac
[ "$arch" = "${EXPECTED_ARCH:?}" ] || fail "image is $arch, expected $EXPECTED_ARCH (rebuild with make toolchain)"

clang --version | head -1 | grep -q -- "-wasi-sdk" || fail "clang is not wasi-sdk's: $(clang --version | head -1)"
[ "$(go version | awk '{print $3}')" = "go$GO_VERSION" ] || fail "$(go version)"
bison --version | head -1 | grep -q " 3.8.2$" || fail "$(bison --version | head -1)"
[ -x /usr/sbin/zic ] || fail "zic is missing"
echo "$PG_SHA256  /opt/src/postgresql-$PG_VERSION.tar.bz2" | sha256sum -c - >/dev/null ||
	fail "Postgres source checksum mismatch"

echo "toolchain ok: $arch, $(clang --version | head -1), go$GO_VERSION, postgres $PG_VERSION"
