#!/usr/bin/env bash
# Builds postgres.wasm, initdb.wasm and share.tar into OUT_DIR (an absolute
# path under /repo). Runs inside the toolchain image (make wasm).
#
# The work tree must not be on the /repo bind mount. On macOS its timestamps
# skew against the VM clock, and make then stops early without an error.
set -euo pipefail
export LC_ALL=C
source /repo/build/versions.env
source /repo/build/wasm-env.sh

OUT=${1:?usage: build-postgres.sh OUT_DIR}
# Overriding PGMEM_WORK changes the output bytes, because the configure
# arguments embed the path. Reproducible builds use the default.
WORK=$(realpath -m -- "${PGMEM_WORK:-/tmp/pgmem-work}")
case "$WORK" in
	/ | /repo | /repo/*)
		echo "build-postgres.sh: work directory $WORK is on the bind mount; use a path outside /repo" >&2
		exit 1
		;;
esac
JOBS=${MAKE_JOBS:-$(nproc)}
PG=$WORK/postgresql-$PG_VERSION
STAGE=$WORK/stage

# make_or_die LOG ARGS... runs make. On error it prints the failure and the log's tail.
make_or_die() {
	local log=$1
	shift
	if ! make "$@" >>"$log" 2>&1; then
		echo "make $* failed:" >&2
		grep -n '\*\*\*' "$log" | tail -5 >&2 || true
		tail -30 "$log" >&2
		exit 1
	fi
}

rm -rf "$WORK"
mkdir -p "$WORK" "$OUT"
tar -xjf "/opt/src/postgresql-$PG_VERSION.tar.bz2" -C "$WORK"
cp -R /repo/postgres/overlay/. "$PG/"
for patch in /repo/postgres/patches/*.patch; do
	[ -e "$patch" ] || continue
	patch -d "$PG" -p1 --forward --fuzz=0 --no-backup-if-mismatch --quiet <"$patch"
done
build_compat "$WORK/compat"

cd "$PG"
# --build is fixed so the configure arguments Postgres embeds are the same on
# every host CPU.
./configure --host=wasm32-unknown-wasi --build=x86_64-pc-linux-gnu \
	--with-template=wasi --prefix=/pgmem \
	--with-system-tzdata=/pgmem/share/timezone --disable-rpath \
	--without-readline --without-zlib --without-icu --without-openssl \
	--without-zstd --without-lz4 --without-libxml --without-llvm \
	CC="$WASM_CC" CPP="$WASM_CC -E" AR="$WASM_AR" RANLIB="$WASM_RANLIB" \
	CFLAGS="$WASM_CFLAGS" CPPFLAGS="$WASM_CPPFLAGS" \
	LDFLAGS="-L$WORK/compat $WASM_LDFLAGS" LIBS="$WASM_LIBS" \
	>"$WORK/configure.log" 2>&1 || {
	tail -40 "$WORK/configure.log" >&2
	exit 1
}

LOG=$WORK/make.log
make_or_die "$LOG" -C src/backend generated-headers
# Build src/port and src/common first. A parallel build of src/backend
# alone can need libpgcommon_srv.a before its sub-make has run.
make_or_die "$LOG" -j"$JOBS" -C src/port
make_or_die "$LOG" -j"$JOBS" -C src/common
# Extension libraries linked into postgres, because WASI has no dlopen. The
# build compiles each one with its own names for the per-library entry
# points, which would otherwise collide. compat/src/dl.c resolves them
# through a generated table. An entry is NAME:DIR, or NAME:DIR:OBJ for one
# of several MODULES built in the same directory. OBJ is a single object
# file with no spaces, because the loop splits entries on whitespace.
STATIC_LIBS="plpgsql:src/pl/plpgsql/src dict_snowball:src/backend/snowball btree_gist:contrib/btree_gist"
# The regression suite's C libraries. regress.so is src/test/regress/regress.c,
# and autoinc and refint are two of contrib/spi's one-file MODULES.
STATIC_LIBS="$STATIC_LIBS regress:src/test/regress autoinc:contrib/spi:autoinc.o refint:contrib/spi:refint.o"
# Encoding conversions. initdb's pg_conversion rows name $libdir/<proc>.
for conv in $(sed -n '/^SUBDIRS/,/^$/p' src/backend/utils/mb/conversion_procs/Makefile | tr -d '\\' | sed 's/SUBDIRS *=//'); do
	STATIC_LIBS="$STATIC_LIBS $conv:src/backend/utils/mb/conversion_procs/$conv"
done
EXT_OBJS=
GEN_ARGS=
for lib in $STATIC_LIBS; do
	name=${lib%%:*}
	rest=${lib#*:}
	dir=${rest%%:*}
	objs=
	[ "$rest" != "$dir" ] && objs=${rest#*:}
	mk=Makefile
	[ -f "$dir/GNUmakefile" ] && mk=GNUmakefile
	list=pgmem-objs-$name.txt
	if [ -n "$objs" ]; then
		# Objects of one MODULES directory share a make tree, so rebuild
		# them with this library's names for the entry points.
		rm -f "$dir/$objs"
		make_or_die "$LOG" -C "$dir" -f "$mk" -f /repo/build/objs.mk pgmem-objs \
			OBJS="$objs" PGMEM_OBJS_LIST="$list" \
			COPT="-DPg_magic_func=Pg_magic_func_$name -D_PG_init=_PG_init_$name"
	else
		make_or_die "$LOG" -j"$JOBS" -C "$dir" -f "$mk" -f /repo/build/objs.mk pgmem-objs \
			PGMEM_OBJS_LIST="$list" \
			COPT="-DPg_magic_func=Pg_magic_func_$name -D_PG_init=_PG_init_$name"
	fi
	EXT_OBJS="$EXT_OBJS $(cat "$dir/$list")"
	GEN_ARGS="$GEN_ARGS $name:$dir/$list"
done
/repo/build/gen-static-libs.sh src/backend/utils/fmgr/pgmem_static_libs.c $GEN_ARGS
make_or_die "$LOG" -C src/backend/utils/fmgr pgmem_static_libs.o
EXT_OBJS="$EXT_OBJS $PG/src/backend/utils/fmgr/pgmem_static_libs.o"
# src/backend/Makefile links $(LDFLAGS_EX_BE) into postgres.
make_or_die "$LOG" -j"$JOBS" -C src/backend LDFLAGS_EX_BE="$EXT_OBJS"
# WASI has no shared libraries, so build libpq as a static archive and tell
# initdb's Makefile not to build the shared one.
make_or_die "$LOG" -j"$JOBS" -C src/interfaces/libpq all-static-lib
make_or_die "$LOG" -j"$JOBS" -C src/fe_utils
make_or_die "$LOG" -j"$JOBS" -C src/bin/initdb \
	-o submake-libpq -o submake-libpgfeutils -o submake-generated-headers initdb

make_or_die "$LOG" -C src/backend install DESTDIR="$STAGE"
make_or_die "$LOG" -C src/timezone/tznames install DESTDIR="$STAGE"
# initdb's inputs that "make -C src/backend install" does not install.
make_or_die "$LOG" -C src/include/catalog install DESTDIR="$STAGE"
make_or_die "$LOG" -C src/pl/plpgsql/src install-data DESTDIR="$STAGE"
make_or_die "$LOG" -C src/backend/snowball install-script DESTDIR="$STAGE"
cp src/backend/snowball/stopwords/*.stop "$STAGE/pgmem/share/postgresql/tsearch_data/"
# contrib's PGXS install would also try to build a shared library.
cp contrib/btree_gist/btree_gist.control contrib/btree_gist/btree_gist--*.sql \
	"$STAGE/pgmem/share/postgresql/extension/"
# dfmgr.c stat()s a library before dlopen; empty placeholders stand in.
mkdir -p "$STAGE/pgmem/lib/postgresql"
for lib in $STATIC_LIBS; do
	: >"$STAGE/pgmem/lib/postgresql/${lib%%:*}.so"
done
/usr/sbin/zic -d "$STAGE/pgmem/share/timezone" src/timezone/data/tzdata.zi

cp src/backend/postgres "$OUT/postgres.wasm"
cp src/bin/initdb/initdb "$OUT/initdb.wasm"
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
	-C "$STAGE/pgmem" -cf "$OUT/share.tar" share lib
cd "$OUT"
sha256sum postgres.wasm initdb.wasm share.tar >SHA256SUMS
cat SHA256SUMS
