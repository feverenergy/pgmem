/*
 * errno is thread-local in wasi-libc. With the wrong target/feature mix the
 * TLS base is never set up and this read traps.
 */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>

int
main(void)
{
	int			fd = open("/definitely/missing", O_RDONLY);
	int			e = errno;

	printf("open=%d errno=%d\n", fd, e);
	return e == ENOENT ? 0 : 1;
}
