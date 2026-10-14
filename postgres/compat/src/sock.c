/*
 * Unix-domain stream sockets over the pgmem kernel (pgm.sock_*). Only
 * AF_UNIX + SOCK_STREAM exists. Data moves through wasi-libc's own recv,
 * send, read, write and shutdown, which the kernel's WASI layer serves; this
 * file creates, names and connects sockets. accept and accept4 are defined
 * here together because wasi-libc defines both in one object file: defining
 * only one would link the other's object and clash.
 */
#include <errno.h>
#include <limits.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>
#include <unistd.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("sock_open") int32_t pgm_sock_open(int32_t flags);
PGM_IMPORT("sock_bind") int32_t pgm_sock_bind(int32_t fd, const char *path, uint32_t len);
PGM_IMPORT("sock_listen") int32_t pgm_sock_listen(int32_t fd, int32_t backlog);
PGM_IMPORT("sock_connect") int32_t pgm_sock_connect(int32_t fd, const char *path, uint32_t len);
PGM_IMPORT("sock_accept") int32_t pgm_sock_accept(int32_t fd, int32_t flags);
PGM_IMPORT("sock_name") int32_t pgm_sock_name(int32_t fd, int32_t which, char *buf, uint32_t buflen);

#define PGM_SOCK_NONBLOCK 1
#define PGM_SOCK_CLOEXEC 2
#define SOCK_BUF (256 * 1024)	/* wasi.SockCapacity */
#define SUN_PATH_MAX (sizeof(((struct sockaddr_un *) 0)->sun_path) - 1)

static int
fail(int32_t r)
{
	errno = -r;
	return -1;
}

static int32_t
pgm_flags(int flags)
{
	return ((flags & SOCK_NONBLOCK) ? PGM_SOCK_NONBLOCK : 0) |
		((flags & SOCK_CLOEXEC) ? PGM_SOCK_CLOEXEC : 0);
}

int
socket(int domain, int type, int protocol)
{
	int32_t		r;

	if (domain != AF_UNIX)
	{
		errno = EAFNOSUPPORT;
		return -1;
	}
	if ((type & ~(SOCK_NONBLOCK | SOCK_CLOEXEC)) != SOCK_STREAM)
	{
		errno = EPROTOTYPE;
		return -1;
	}
	if (protocol != 0)
	{
		errno = EPROTONOSUPPORT;
		return -1;
	}
	r = pgm_sock_open(pgm_flags(type));
	return r < 0 ? fail(r) : r;
}

/*
 * unix_path extracts addr's path into out (absolute: relative paths are
 * resolved against the working directory) and returns its length, or -1
 * with errno set.
 */
static int
unix_path(const struct sockaddr *addr, socklen_t len, char out[PATH_MAX])
{
	const struct sockaddr_un *un = (const struct sockaddr_un *) addr;
	size_t		n,
				max;
	size_t		cwdlen = 0;

	if (addr == NULL || len < offsetof(struct sockaddr_un, sun_path) + 1 || un->sun_family != AF_UNIX)
	{
		errno = addr != NULL && len >= sizeof(sa_family_t) && un->sun_family != AF_UNIX ? EAFNOSUPPORT : EINVAL;
		return -1;
	}
	max = len - offsetof(struct sockaddr_un, sun_path);
	n = strnlen(un->sun_path, max);
	if (n == 0 || n > SUN_PATH_MAX)
	{
		errno = EINVAL;
		return -1;
	}
	if (un->sun_path[0] != '/')
	{
		if (getcwd(out, PATH_MAX) == NULL)
			return -1;
		cwdlen = strlen(out);
		if (cwdlen + 1 + n >= PATH_MAX)
		{
			errno = ENAMETOOLONG;
			return -1;
		}
		if (cwdlen > 1)
			out[cwdlen++] = '/';
		else
			cwdlen = 1;			/* cwd is "/" */
	}
	memcpy(out + cwdlen, un->sun_path, n);
	out[cwdlen + n] = '\0';
	return (int) (cwdlen + n);
}

int
bind(int fd, const struct sockaddr *addr, socklen_t len)
{
	char		path[PATH_MAX];
	int			n = unix_path(addr, len, path);
	int32_t		r;

	if (n < 0)
		return -1;
	r = pgm_sock_bind(fd, path, (uint32_t) n);
	return r < 0 ? fail(r) : 0;
}

int
listen(int fd, int backlog)
{
	int32_t		r = pgm_sock_listen(fd, backlog);

	return r < 0 ? fail(r) : 0;
}

int
connect(int fd, const struct sockaddr *addr, socklen_t len)
{
	char		path[PATH_MAX];
	int			n = unix_path(addr, len, path);
	int32_t		r;

	if (n < 0)
		return -1;
	r = pgm_sock_connect(fd, path, (uint32_t) n);
	return r < 0 ? fail(r) : 0;
}

/* fill_addr writes fd's local (which 0) or peer (1) address to addr. */
static int
fill_addr(int fd, int which, struct sockaddr *addr, socklen_t *len)
{
	struct sockaddr_un un;
	int32_t		r;
	socklen_t	full;

	memset(&un, 0, sizeof(un));
	un.sun_family = AF_UNIX;
	r = pgm_sock_name(fd, which, un.sun_path, SUN_PATH_MAX);
	if (r < 0)
		return fail(r);
	full = (socklen_t) (offsetof(struct sockaddr_un, sun_path) + (r > 0 ? (size_t) r + 1 : 0));
	if (addr != NULL && len != NULL)
	{
		memcpy(addr, &un, *len < full ? *len : full);
		*len = full;
	}
	return 0;
}

int
accept4(int fd, struct sockaddr *restrict addr, socklen_t *restrict len, int flags)
{
	int32_t		r;

	if (flags & ~(SOCK_NONBLOCK | SOCK_CLOEXEC))
	{
		errno = EINVAL;
		return -1;
	}
	r = pgm_sock_accept(fd, pgm_flags(flags));
	if (r < 0)
		return fail(r);
	if (addr != NULL && fill_addr(r, 1, addr, len) < 0)
	{
		int			e = errno;

		close(r);
		errno = e;
		return -1;
	}
	return r;
}

int
accept(int fd, struct sockaddr *restrict addr, socklen_t *restrict len)
{
	return accept4(fd, addr, len, 0);
}

int
getsockname(int fd, struct sockaddr *restrict addr, socklen_t *restrict len)
{
	return fill_addr(fd, 0, addr, len);
}

int
getpeername(int fd, struct sockaddr *restrict addr, socklen_t *restrict len)
{
	return fill_addr(fd, 1, addr, len);
}

/* is_socket fails with ENOTSOCK (or fstat's errno) unless fd is a socket. */
static int
is_socket(int fd)
{
	struct stat st;

	if (fstat(fd, &st) < 0)
		return -1;
	if (!S_ISSOCK(st.st_mode))
	{
		errno = ENOTSOCK;
		return -1;
	}
	return 0;
}

int
getsockopt(int fd, int level, int name, void *restrict value, socklen_t *restrict len)
{
	int			v;

	if (is_socket(fd) < 0)
		return -1;
	if (level != SOL_SOCKET)
	{
		errno = ENOPROTOOPT;
		return -1;
	}
	switch (name)
	{
		case SO_TYPE:
			v = SOCK_STREAM;
			break;
		case SO_ERROR:
			v = 0;
			break;
		case SO_SNDBUF:
		case SO_RCVBUF:
			v = SOCK_BUF;
			break;
		default:
			errno = ENOPROTOOPT;
			return -1;
	}
	if (value == NULL || len == NULL || *len < sizeof(int))
	{
		errno = EINVAL;
		return -1;
	}
	memcpy(value, &v, sizeof(int));
	*len = sizeof(int);
	return 0;
}

/*
 * setsockopt accepts and ignores the options Postgres and clients set on
 * any socket; buffer sizes are fixed.
 */
int
setsockopt(int fd, int level, int name, const void *value, socklen_t len)
{
	(void) value;
	(void) len;
	if (is_socket(fd) < 0)
		return -1;
	if (level == SOL_SOCKET && (name == SO_REUSEADDR || name == SO_KEEPALIVE ||
								name == SO_SNDBUF || name == SO_RCVBUF))
		return 0;
	errno = ENOPROTOOPT;
	return -1;
}
