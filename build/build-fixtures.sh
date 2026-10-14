#!/usr/bin/env bash
# Compiles the C test programs into committed .wasm fixtures. Runs inside the
# toolchain image (make fixtures).
set -euo pipefail
export LC_ALL=C
source /repo/build/wasm-env.sh

# The fixtures are committed, so they leave out the DWARF that wasi-libc's
# objects carry. The name section stays, for wazero's stack traces.
WASM_LDFLAGS="$WASM_LDFLAGS -Wl,--strip-debug"

# pgmem's WASI layer, tested without the compat layer or the kernel.
cd /repo/internal/wasi/testdata
for src in c/*.c; do
	$WASM_CC $WASM_CFLAGS -Wall -Werror "$src" $WASM_LDFLAGS -o "$(basename "$src" .c).wasm"
done

# The compat layer's own test, linked against the compat library.
build_compat /tmp/pgmem-compat
mkdir -p /repo/postgres/compat/testdata
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Wextra -Werror /repo/postgres/compat/test/compat_test.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o /repo/postgres/compat/testdata/compat_test.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Wextra -Werror /repo/postgres/compat/test/shell_test.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o /repo/postgres/compat/testdata/shell_test.wasm

# The wasmrt fixtures link the compat layer too, so getpid and the pgm ABI are uniform.
cd /repo/internal/wasmrt/testdata
for src in c/*.c; do
	$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror "$src" \
		-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o "$(basename "$src" .c).wasm"
done

# The kernel fixtures procs, signals, fds, bare, spin, shm, sem and sock link
# the compat layer. noabi does not, on purpose.
cd /repo/internal/kernel/testdata
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/procs.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o procs.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/signals.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o signals.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/fds.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o fds.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/bare.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o bare.wasm
$WASM_CC $WASM_CFLAGS -Wall -Werror c/noabi.c $WASM_LDFLAGS -o noabi.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/spin.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o spin.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/shm.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o shm.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/sem.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o sem.wasm
$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Werror c/sock.c \
	-L/tmp/pgmem-compat $WASM_LDFLAGS $WASM_LIBS -o sock.wasm

# wasm-ld marks its output executable. Git records the fixtures as plain
# files, so make check's diff sees no mode change.
chmod 644 /repo/internal/wasi/testdata/*.wasm /repo/postgres/compat/testdata/*.wasm \
	/repo/internal/wasmrt/testdata/*.wasm /repo/internal/kernel/testdata/*.wasm
