/* The table of statically linked extension libraries (see compat/src/dl.c). */
#ifndef PGMEM_STATIC_LIBS_H
#define PGMEM_STATIC_LIBS_H

struct pgmem_static_symbol
{
	const char *name;
	void	   *addr;
};

struct pgmem_static_library
{
	const char *name;			/* base name without suffix, e.g. "plpgsql" */
	const struct pgmem_static_symbol *symbols;	/* NULL-name terminated */
};

#endif
