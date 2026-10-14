/* There is no terminal; time zones come from Postgres's own pg_tz. */
#include <errno.h>
#include <termios.h>

int
tcgetattr(int fd, struct termios *t)
{
	(void) fd;
	(void) t;
	errno = ENOTTY;
	return -1;
}

int
tcsetattr(int fd, int action, const struct termios *t)
{
	(void) fd;
	(void) action;
	(void) t;
	errno = ENOTTY;
	return -1;
}

void
tzset(void)
{
}
