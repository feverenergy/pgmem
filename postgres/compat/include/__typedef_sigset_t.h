/*
 * Replaces wasi-libc's one-byte placeholder: Postgres uses signals up to 31.
 * No wasi-libc function we link takes a sigset_t, so widening is safe.
 */
#ifndef __wasilibc___typedef_sigset_t_h
#define __wasilibc___typedef_sigset_t_h
typedef unsigned long long sigset_t;
#endif
