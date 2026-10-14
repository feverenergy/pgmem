/*
 * Computes without host calls: forever (mode "forever") or for a while.
 * Mode "yield" computes forever but polls like Postgres's
 * CHECK_FOR_INTERRUPTS (PGMEM_CFI_POLL, pgmem_compat.h), which makes a host
 * call every 4096th iteration; it prints "ready" as it starts.
 */
#include <stdint.h>
#include <stdio.h>
#include <string.h>

int
main(int argc, char **argv)
{
	volatile uint64_t x = 88172645463325252ULL;
	const char *mode = argc > 1 ? argv[1] : "";
	uint64_t	n = 400000000ULL;

	if (strcmp(mode, "yield") == 0)
	{
		/* Tell the test the loop is about to start. */
		printf("ready\n");
		fflush(stdout);
		for (;;)
		{
			x ^= x << 13;
			x ^= x >> 7;
			x ^= x << 17;
			PGMEM_CFI_POLL();
		}
	}
	if (strcmp(mode, "forever") == 0)
		n = UINT64_MAX;
	for (uint64_t i = 0; i < n; i++)
	{
		x ^= x << 13;
		x ^= x >> 7;
		x ^= x << 17;
	}
	printf("%llu\n", (unsigned long long) (x & 1));
	return 0;
}
