/* Imports from "pgm" without the compat layer, so it has no ABI export. */
#include <stdint.h>

__attribute__((import_module("pgm"), import_name("getpid"))) int32_t pgm_getpid(void);

int
main(void)
{
	return pgm_getpid() > 0 ? 0 : 1;
}
