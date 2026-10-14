# Compiler settings shared by every pgmem WebAssembly build. The build
# scripts inside the toolchain image source this file.
WASI_SDK=/opt/wasi-sdk
# setjmp/longjmp become WebAssembly exceptions, in the exnref encoding that
# wazero accepts. The flags are part of the compiler command so that every
# use, including configure's preprocessor checks, sees them. wasi-libc's
# setjmp.h refuses to compile without them.
WASM_CC="$WASI_SDK/bin/clang --target=wasm32-wasip1-threads -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false"
WASM_AR="$WASI_SDK/bin/llvm-ar"
WASM_RANLIB="$WASI_SDK/bin/llvm-ranlib"
WASM_CFLAGS="-O2"
# wasm-ld's default 64 KB stack is far too small for Postgres.
WASM_LDFLAGS="-Wl,--max-memory=4294967296 -Wl,-z,stack-size=8388608"

# The compat layer (postgres/compat) supplies the POSIX pieces wasi-libc
# lacks. Its include directory must come before the sysroot because three of
# its headers replace wasi-libc's.
COMPAT_DIR=/repo/postgres/compat
WASM_CPPFLAGS="-D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_MMAN -D_WASI_EMULATED_GETPID -I$COMPAT_DIR/include -include pgmem_compat.h"
# WASM_LIBS leaves out libwasi-emulated-signal on purpose, because the
# compat layer defines signal(), raise(), __SIG_IGN and __SIG_ERR itself.
# It also leaves out libwasi-emulated-getpid. Process IDs come from the
# pgmem kernel, and the compat layer's getpid() asks it.
# -D_WASI_EMULATED_GETPID above stays, because it makes unistd.h declare
# getpid(). -u pgmem_abi_version links the compat layer's ABI version
# export into every program, whether or not the program refers to it. The
# kernel refuses a "pgm" importer without it. The export keeps abi.o live,
# and abi.o anchors the other pieces every program needs
# (postgres/compat/src/abi.c). -u alone would not keep those, because
# wasm-ld drops an undefined-but-unused symbol's definition, and with it a
# constructor in the same object file.
WASM_LIBS="-lpgmemcompat -lsetjmp -lwasi-emulated-process-clocks -lwasi-emulated-mman -Wl,-u,pgmem_abi_version"

# build_compat DIR compiles the compat layer into DIR/libpgmemcompat.a.
build_compat() {
	local dir=$1 src
	mkdir -p "$dir"
	rm -f "$dir"/*.o "$dir/libpgmemcompat.a"
	for src in "$COMPAT_DIR"/src/*.c; do
		$WASM_CC $WASM_CFLAGS $WASM_CPPFLAGS -Wall -Wextra -Werror -c "$src" -o "$dir/$(basename "$src" .c).o"
	done
	"$WASM_AR" rcs "$dir/libpgmemcompat.a" "$dir"/*.o
}
