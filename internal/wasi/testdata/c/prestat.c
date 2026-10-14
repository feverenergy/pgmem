/* fd_prestat_get must answer EBADF for open fds that are not preopens, or
 * wasi-libc's preopen scan aborts the program at startup. */
#include <stdio.h>
#include <wasi/api.h>

int
main(void)
{
	__wasi_prestat_t p;

	printf("prestat(4)=%d prestat(3)=%d\n", __wasi_fd_prestat_get(4, &p), __wasi_fd_prestat_get(3, &p));
	return 0;
}
