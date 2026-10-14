/*
 * dlopen without dynamic linking. Extension libraries are linked into
 * postgres.wasm; a generated table (pgmem_static_libraries, see
 * build/gen-static-libs.sh) lists each library's exported symbols. dlopen
 * matches a path by its base name without the suffix, so
 * "/pgmem/lib/postgresql/plpgsql.so" finds "plpgsql". dfmgr.c stat()s the
 * path first, so the file must exist (an empty placeholder is enough).
 */
#include <dlfcn.h>
#include <stddef.h>
#include <string.h>

#include "pgmem_static_libs.h"

/* Defined only in binaries that link extensions (postgres). */
extern const struct pgmem_static_library pgmem_static_libraries[] __attribute__((weak));

static const char *last_error;

void *
dlopen(const char *path, int mode)
{
	const char *base;
	size_t		len;
	const struct pgmem_static_library *lib;

	(void) mode;
	if (path == NULL)
	{
		last_error = "dlopen(NULL) is not supported";
		return NULL;
	}
	base = strrchr(path, '/');
	base = base ? base + 1 : path;
	len = strcspn(base, ".");
	for (lib = pgmem_static_libraries; lib && lib->name; lib++)
		if (strlen(lib->name) == len && strncmp(lib->name, base, len) == 0)
			return (void *) lib;
	last_error = "library is not linked into this pgmem build";
	return NULL;
}

void *
dlsym(void *handle, const char *name)
{
	const struct pgmem_static_library *lib = handle;
	const struct pgmem_static_symbol *sym;

	if (handle == NULL)
	{
		last_error = "dlsym: NULL handle";
		return NULL;
	}
	for (sym = lib->symbols; sym->name; sym++)
		if (strcmp(sym->name, name) == 0)
			return sym->addr;
	last_error = "symbol not found";
	return NULL;
}

int
dlclose(void *handle)
{
	(void) handle;
	return 0;
}

char *
dlerror(void)
{
	const char *e = last_error;

	last_error = NULL;
	return (char *) e;
}
