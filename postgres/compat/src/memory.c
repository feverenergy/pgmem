/*
 * Postgres calls msync only as a writeback hint (pg_flush_data). pgmem's
 * files live in memory, so there is nothing to flush.
 */
#include <stddef.h>
#include <sys/mman.h>

int
msync(void *addr, size_t len, int flags)
{
	(void) addr;
	(void) len;
	(void) flags;
	return 0;
}
