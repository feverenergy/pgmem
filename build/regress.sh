#!/usr/bin/env bash
# Runs PostgreSQL's parallel_schedule against pgmem (make regress), then
# checks the failures against postgres/regress-allowlist. Usage:
#
#	build/regress.sh [OUT [pg_regress args...]]   (default OUT: build/out)
#
# It needs make wasm cluster and build/build-native-regress.sh OUT first.
# Results go to OUT/regress/: regression.diffs, results/, the server log
# and pgmem-serve's -stats. Extra pg_regress arguments replace the
# schedule, to debug one test. The allowlist check then reports the listed
# tests that did not run.
#
# pgmem-serve listens only on a Unix socket in a new /tmp/pgmr.XXXX
# directory. It opens no TCP port, and the script unsets every PG*
# connection variable, so nothing can reach a developer's own server.
# pgmem-serve's RSS watchdog stops the run past -max-rss-mb, which is 4096
# unless REGRESS_MAX_RSS_MB overrides it.
set -euo pipefail

# libpq reads PGHOST, PGPORT, PGSERVICE, PGPASSFILE and the rest, so unset
# every PG* variable except pgmem's own PGMEM_*.
for v in $(compgen -e); do
	case "$v" in PGMEM_*) ;; PG*) unset "$v" ;; esac
done

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=versions.env
. "$REPO/build/versions.env"
BUILD=$(cd "${1:-$REPO/build/out}" && pwd)
shift || true
GOOS=$(go env GOOS)
N=$BUILD/native-$GOOS-$(go env GOARCH)
SRC=$N/postgresql-$PG_VERSION/src/test/regress
BIN=$N/install/bin
OUT=$BUILD/regress
PORT=54329
for f in "$BUILD/postgres.wasm" "$BUILD/cluster.tar"; do
	[ -f "$f" ] || { echo "regress: $f is missing; run make wasm cluster" >&2; exit 1; }
done
for f in "$BIN/psql" "$SRC/pg_regress"; do
	[ -x "$f" ] || { echo "regress: $f is missing; run build/build-native-regress.sh" >&2; exit 1; }
done

rm -rf "$OUT"
mkdir -p "$OUT/results" "$OUT/bin"
(cd "$REPO" && go build -o "$OUT/bin/" ./cmd/pgmem-serve ./internal/cmd/regress-check)

SOCK=$(mktemp -d /tmp/pgmr.XXXX)
SERVE=
cleanup() {
	if [ -n "$SERVE" ]; then
		kill -INT "$SERVE" 2>/dev/null || true
		wait "$SERVE" 2>/dev/null || true
	fi
	rm -rf "$SOCK"
}
trap cleanup EXIT

# pg_regress passes its platform's library suffix (PG_DLSUFFIX). On macOS
# that is .dylib, so pgmem needs placeholders under that name too.
LIBSUFFIX=()
[ "$GOOS" = darwin ] && LIBSUFFIX=(-lib-suffix .dylib)
PGMEM_BUILD_DIR=$BUILD "$OUT/bin/pgmem-serve" -socket "$SOCK" -port "$PORT" \
	-in "$SRC/data" -out "$OUT/results" "${LIBSUFFIX[@]}" \
	-set "log_line_prefix=%m [%p] %a " -set log_min_duration_statement=2000 \
	-max-rss-mb "${REGRESS_MAX_RSS_MB:-4096}" \
	-log "$OUT/server.log" -stats "$OUT/stats.txt" >"$OUT/serve.out" 2>"$OUT/serve.err" &
SERVE=$!
for _ in $(seq 600); do
	grep -q '^ready ' "$OUT/serve.out" && break
	if ! kill -0 "$SERVE" 2>/dev/null; then
		wait "$SERVE" || true
		SERVE=
		echo "regress: pgmem-serve exited before it was ready:" >&2
		cat "$OUT/serve.err" >&2
		exit 1
	fi
	sleep 0.1
done
grep -q '^ready ' "$OUT/serve.out" || { echo "regress: pgmem-serve not ready after 60 s" >&2; exit 1; }

PSQL=("$BIN/psql" -X -h "$SOCK" -p "$PORT" -U postgres -d postgres -q -v ON_ERROR_STOP=1)
# --use-existing skips pg_regress's create_database(), so run its SQL here.
"${PSQL[@]}" <<'SQL'
DROP DATABASE IF EXISTS "regression";
CREATE DATABASE "regression" TEMPLATE=template0;
ALTER DATABASE "regression" SET lc_messages TO 'C';
ALTER DATABASE "regression" SET lc_monetary TO 'C';
ALTER DATABASE "regression" SET lc_numeric TO 'C';
ALTER DATABASE "regression" SET lc_time TO 'C';
ALTER DATABASE "regression" SET bytea_output TO 'hex';
ALTER DATABASE "regression" SET timezone_abbreviations TO 'Default';
SQL

[ $# -gt 0 ] || set -- --schedule="$SRC/parallel_schedule"
start=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
set +e
"$SRC/pg_regress" --use-existing --host="$SOCK" --port="$PORT" --user=postgres \
	--bindir="$BIN" --inputdir="$SRC" --expecteddir="$SRC" --outputdir="$OUT" \
	--dlpath=/pgmem/lib/postgresql --max-concurrent-tests=20 "$@" 2>&1 | tee "$OUT/pg_regress.out"
rc=${PIPESTATUS[0]}
set -e
wall=$(perl -MTime::HiRes=time -e "printf '%.1f', time - $start")

# Stop pgmem-serve, which writes the -stats summary as it exits. It is
# already gone if its watchdog tripped.
kill -INT "$SERVE" 2>/dev/null || true
serve_rc=0
wait "$SERVE" || serve_rc=$?
SERVE=
echo "regress: pg_regress exit $rc; wall $wall s"
# The GC pause and stall summary, the worst stall and the peak RSS.
awk '(/ FINAL / && !/stopping|other:/) || (/ WORST STALL / && !w++) { $1 = $2 = ""; sub(/^  /, ""); print "regress: " $0 }' "$OUT/stats.txt" || true
if [ "$serve_rc" -ne 0 ]; then
	echo "regress: pgmem-serve exit $serve_rc:" >&2
	cat "$OUT/serve.err" >&2
	exit 1
fi
if [ "$rc" -ne 0 ] && [ "$rc" -ne 1 ]; then
	echo "regress: pg_regress failed (exit $rc)" >&2
	exit 1
fi
# pg_regress exits 1 when a test failed. regress-check then refuses a run
# whose regression.out is missing instead of calling it a pass.
check=0
"$OUT/bin/regress-check" -allowlist "$REPO/postgres/regress-allowlist" -exit "$rc" "$OUT" || check=$?
# A backend crash fails the run even when every diff is allowlisted. The
# kernel's trap line or the postmaster's report of a signal death shows one.
crashes=$(grep -E 'pgmem: .* trapped: |terminated by signal' "$OUT/server.log" || true)
if [ -n "$crashes" ]; then
	echo "regress: FAIL: the server log shows a backend crash:" >&2
	echo "$crashes" | head -20 >&2
	exit 1
fi
exit "$check"
