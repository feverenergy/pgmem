/*
 * poll() and non-blocking reads on a pipe whose read end is fd 4 and write
 * end fd 5 (installed by the test). Prints "poll ok".
 */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)

int
main(void)
{
	char		c;
	struct pollfd rd = {.fd = 4, .events = POLLIN};
	struct pollfd wr = {.fd = 5, .events = POLLOUT};
	int			flags = fcntl(4, F_GETFL);

	CHECK(flags >= 0 && fcntl(4, F_SETFL, flags | O_NONBLOCK) == 0);
	CHECK(read(4, &c, 1) == -1 && errno == EAGAIN);
	CHECK(poll(&rd, 1, 50) == 0);	/* nothing to read: the timeout fires */
	CHECK(poll(&wr, 1, 0) == 1 && (wr.revents & POLLOUT));
	CHECK(write(5, "x", 1) == 1);
	CHECK(poll(&rd, 1, -1) == 1 && (rd.revents & POLLIN));
	CHECK(read(4, &c, 1) == 1 && c == 'x');
	CHECK(lseek(4, 0, SEEK_CUR) == -1 && errno == ESPIPE);
	CHECK(close(5) == 0);
	rd.revents = 0;
	CHECK(poll(&rd, 1, -1) == 1);	/* writer gone: readable with hang-up */
	CHECK(read(4, &c, 1) == 0);		/* EOF */
	puts("poll ok");
	return 0;
}
