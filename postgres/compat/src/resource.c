/* Resource limits reported to Postgres. The stack limit matches the link flag -z stack-size=8388608. */
#include <errno.h>

#define PGMEM_STACK_BYTES (8ULL * 1024 * 1024)
/* The kernel's fd ceiling, wasi.MaxFiles in internal/wasi/fdtable.go. */
#define PGMEM_MAX_FILES 4096ULL

int
getrlimit(int resource, struct rlimit *rlim)
{
	switch (resource)
	{
		case RLIMIT_STACK:
			rlim->rlim_cur = rlim->rlim_max = PGMEM_STACK_BYTES;
			return 0;
		case RLIMIT_NOFILE:
			rlim->rlim_cur = rlim->rlim_max = PGMEM_MAX_FILES;
			return 0;
		case RLIMIT_CORE:
			rlim->rlim_cur = rlim->rlim_max = 0;
			return 0;
		default:
			rlim->rlim_cur = rlim->rlim_max = RLIM_INFINITY;
			return 0;
	}
}

int
setrlimit(int resource, const struct rlimit *rlim)
{
	(void) resource;
	(void) rlim;
	return 0;
}
