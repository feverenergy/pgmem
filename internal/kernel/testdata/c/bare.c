/*
 * Calls no timer or process function of the compat layer, so only the -u
 * flags in build/wasm-env.sh link in its clock_nanosleep and its startup
 * chdir. Mode "cwd" (spawned by fds from /work/sub) checks the inherited
 * working directory; mode "sleep" says it is ready on stdout, then its
 * nanosleep must fail with EINTR when SIGUSR1 comes.
 */
#include <errno.h>
#include <signal.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

static volatile sig_atomic_t got;

static void
on_usr1(int signo)
{
	(void) signo;
	got = 1;
}

int
main(int argc, char **argv)
{
	char		cwd[256];

	if (argc > 1 && strcmp(argv[1], "cwd") == 0)
		return getcwd(cwd, sizeof(cwd)) != NULL && strcmp(cwd, "/work/sub") == 0 ? 0 : 1;
	if (argc > 1 && strcmp(argv[1], "sleep") == 0)
	{
		struct sigaction sa = {0};
		struct timespec ts = {.tv_sec = 10};

		sa.sa_handler = on_usr1;
		if (sigaction(SIGUSR1, &sa, NULL) != 0 || write(1, "r", 1) != 1)
			return 2;
		if (nanosleep(&ts, NULL) != -1 || errno != EINTR || !got)
			return 3;
		return 0;
	}
	return 97;
}
