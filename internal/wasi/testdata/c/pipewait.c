/* Blocking read of fd 4 until EOF; prints the byte count. */
#include <stdio.h>
#include <unistd.h>

int
main(void)
{
	char		buf[4096];
	long		total = 0;
	ssize_t		n;

	while ((n = read(4, buf, sizeof(buf))) > 0)
		total += n;
	printf("%ld %s\n", total, n == 0 ? "eof" : "error");
	return 0;
}
