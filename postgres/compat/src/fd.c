/*
 * Close-on-exec and fd duplication, implemented by the pgmem kernel through
 * the "pgm" host module.
 *
 * wasi-libc has no close-on-exec: O_CLOEXEC is 0, fcntl(F_GETFD) always
 * answers FD_CLOEXEC and F_SETFD does nothing, and dup, dup2 and F_DUPFD
 * fail. Under pgmem, children inherit every fd that is not close-on-exec at
 * the same number (pgmem_spawn), so the kernel keeps the flag per fd, and
 * these replace wasi-libc's. pgmem_compat.h routes fcntl here.
 */
#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdint.h>
#include <unistd.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("dup") int32_t pgm_dup(int32_t fd, int32_t min, int32_t cloexec);
PGM_IMPORT("dup2") int32_t pgm_dup2(int32_t fd, int32_t newfd);
PGM_IMPORT("fd_getflags") int32_t pgm_fd_getflags(int32_t fd);
PGM_IMPORT("fd_setflags") int32_t pgm_fd_setflags(int32_t fd, int32_t flags);

static int
host_result(int32_t r)
{
	if (r < 0)
	{
		errno = -r;
		return -1;
	}
	return r;
}

/*
 * fcntl with F_GETFD, F_SETFD, F_DUPFD and F_DUPFD_CLOEXEC answered by the
 * kernel; every other command is wasi-libc's. The third argument is read
 * only for commands that take one: an int, or for wasi-libc's other
 * commands possibly a pointer, which is no wider on wasm32.
 */
#undef fcntl
int
pgmem_fcntl(int fd, int cmd,...)
{
	va_list		ap;
	int			arg = 0;

	if (cmd != F_GETFD && cmd != F_GETFL)
	{
		va_start(ap, cmd);
		arg = va_arg(ap, int);
		va_end(ap);
	}

	switch (cmd)
	{
		case F_GETFD:
			return host_result(pgm_fd_getflags(fd));
		case F_SETFD:
			return host_result(pgm_fd_setflags(fd, arg));
		case F_DUPFD:
		case F_DUPFD_CLOEXEC:
			return host_result(pgm_dup(fd, arg, cmd == F_DUPFD_CLOEXEC));
		case F_GETFL:
			return fcntl(fd, cmd);
		default:
			return fcntl(fd, cmd, arg);
	}
}

int
dup(int fd)
{
	return host_result(pgm_dup(fd, 0, 0));
}

int
dup2(int fd, int newfd)
{
	return host_result(pgm_dup2(fd, newfd));
}
