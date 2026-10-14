/*
 * pgmem's host refuses to run a module that imports from "pgm" unless it
 * exports this function returning the ABI version it was built against.
 * build/wasm-env.sh links it into every program with -u.
 */
#include <time.h>

#define PGMEM_ABI_VERSION 5

__attribute__((export_name("pgmem_abi_version")))
int
pgmem_abi_version(void)
{
	return PGMEM_ABI_VERSION;
}

/*
 * Pieces of the compat layer every program needs, whether or not it calls
 * anything else in their object files: the constructor that moves a
 * spawned child to its parent's working directory (process.c), and
 * clock_nanosleep (timer.c), so that a signal interrupts nanosleep, usleep
 * and sleep with EINTR even in a program that never sets a timer. wasm-ld
 * keeps a constructor only when something else in its object file is live;
 * this file always is (its export), and "used" keeps these references, so
 * both object files are linked and live in every program.
 */
void		pgmem_startup_chdir(void);

__attribute__((used))
static const struct
{
	void		(*startup_chdir) (void);
	int			(*clock_nanosleep) (clockid_t, int, const struct timespec *, struct timespec *);
}			anchors = {pgmem_startup_chdir, clock_nanosleep};
