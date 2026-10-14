/* proc_exit's code, and traps, reach the Go caller. */
#include <stdlib.h>
#include <string.h>

int
main(int argc, char **argv)
{
	if (argc > 1 && strcmp(argv[1], "trap") == 0)
		__builtin_trap();
	return argc > 1 ? atoi(argv[1]) : 0;
}
