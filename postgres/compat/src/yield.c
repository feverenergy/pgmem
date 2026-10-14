/*
 * PGMEM_CFI_POLL's host call (pgmem_compat.h).
 *
 * This lives apart from signal.c so that a program that polls but never
 * touches signals (the kernel's spin fixture) does not link the signal
 * block and its dispatch export: without them the kernel carries out a
 * signal's default action itself.
 */
#include <stdint.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("yield") int32_t pgm_yield(void);

uint32_t	pgmem_cfi_ticks;

/*
 * pgm.yield does nothing itself, but like every pgm call it ends a killed
 * process and dispatches pending signals on the way out, and Go can stop
 * the world while the guest is in it.
 */
void
pgmem_cfi_yield(void)
{
	(void) pgm_yield();
}
