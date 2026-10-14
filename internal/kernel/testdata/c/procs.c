/*
 * Process behaviour under the pgmem kernel. Without arguments it runs the
 * checks and prints "procs ok"; the other modes are the children it starts.
 */
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define CHECK_CHILD(cond) do { if (!(cond)) return 98; } while (0)
#define SELF "/fixtures/procs"

extern char **environ;

static int
child(const char *mode)
{
	char		buf[256];
	ssize_t		n;

	if (strcmp(mode, "upper") == 0)
	{
		while ((n = read(0, buf, sizeof(buf))) > 0)
		{
			for (ssize_t i = 0; i < n; i++)
				if (buf[i] >= 'a' && buf[i] <= 'z')
					buf[i] -= 32;
			write(1, buf, n);
		}
		exit(7);
	}
	if (strcmp(mode, "trap") == 0)
		__builtin_trap();
	/* exit codes the kernel must not mistake for a kill */
	if (strcmp(mode, "exit137") == 0)
		exit(137);
	if (strcmp(mode, "exit255") == 0)
		exit(255);
	if (strcmp(mode, "hold") == 0)
	{
		while (read(0, buf, sizeof(buf)) > 0)
			;
		return 0;
	}
	if (strcmp(mode, "fd5") == 0)
	{
		/* fd 5 is inherited above the preopen: libc's startup must cope */
		CHECK_CHILD(write(5, "five", 4) == 4);
		return 0;
	}
	if (strcmp(mode, "sleep") == 0)
	{
		/* blocks in poll_oneoff; returns only if the sleep is interrupted */
		write(1, "ready\n", 6);
		sleep(3600);
		return 0;
	}
	if (strcmp(mode, "spin") == 0)
	{
		/* retries an interrupted sleep forever, as Postgres retries EINTR */
		write(1, "ready\n", 6);
		for (;;)
			sleep(3600);
	}
	if (strcmp(mode, "waitchild") == 0)
	{
		/* blocks in pgm.wait for a sleeping child */
		char	   *cargv[] = {SELF, "sleep", NULL};
		int			map[] = {1, 2};
		int			st;
		pid_t		c = pgmem_spawn(cargv, environ, map, 1);

		CHECK_CHILD(c > 0);
		write(1, "ready\n", 6);
		waitpid(c, &st, 0);
		return 0;
	}
	if (strcmp(mode, "piperead") == 0)
	{
		/* blocks reading a pipe whose write end it holds itself */
		int			p[2];

		CHECK_CHILD(pipe(p) == 0);
		write(1, "ready\n", 6);
		read(p[0], buf, sizeof(buf));
		return 0;
	}
	if (strcmp(mode, "spawnfail") == 0)
	{
		/*
		 * The child cannot be instantiated (a test allocator refuses): spawn
		 * fails with ENOMEM and drops the child's copy of the pipe's write
		 * end, so the read end sees EOF rather than EAGAIN.
		 */
		char	   *cargv[] = {SELF, "info", NULL};
		int			p[2];
		int			map[] = {1, 0};

		CHECK_CHILD(pipe(p) == 0);
		map[1] = p[1];
		CHECK_CHILD(pgmem_spawn(cargv, environ, map, 1) == -1 && errno == ENOMEM);
		CHECK_CHILD(close(p[1]) == 0);
		CHECK_CHILD(fcntl(p[0], F_SETFL, O_NONBLOCK) == 0);
		CHECK_CHILD(read(p[0], buf, sizeof(buf)) == 0);
		return 0;
	}
	if (strcmp(mode, "bigargv") == 0)
	{
		/*
		 * argv and environment may total 2 MiB, counting each string's NUL:
		 * here SELF (16), "info" (5) and big.
		 */
		size_t		limit = 2 << 20;
		char	   *big = malloc(limit);
		char	   *cargv[] = {SELF, "info", big, NULL};
		char	   *cenv[] = {NULL};
		int			map[] = {1, 2};
		int			st;
		pid_t		c;

		CHECK_CHILD(big != NULL);
		memset(big, 'x', limit - 16 - 5 - 1);
		big[limit - 16 - 5 - 1] = '\0';
		c = pgmem_spawn(cargv, cenv, map, 1);
		CHECK_CHILD(c > 0 && waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
		big[limit - 16 - 5 - 1] = 'x';
		big[limit - 16 - 5] = '\0';
		CHECK_CHILD(pgmem_spawn(cargv, cenv, map, 1) == -1 && errno == E2BIG);
		free(big);
		return 0;
	}
	if (strcmp(mode, "info") == 0)
	{
		const char *v = getenv("PGMEM_TEST");

		printf("%d %s", (int) getppid(), v ? v : "-");
		return 0;
	}
	return 99;
}

static ssize_t
read_all(int fd, char *buf, size_t cap)
{
	size_t		got = 0;
	ssize_t		n;

	while (got < cap && (n = read(fd, buf + got, cap - got)) > 0)
		got += n;
	return got;
}


/*
 * pipe with both ends close-on-exec, as a POSIX program must make the ends
 * it keeps: a child inherits every other fd, and one holding the write end
 * of its own input would never see EOF. The fd map still gives a child the
 * end it is meant to have.
 */
static int
cloexec_pipe(int fds[2])
{
	if (pipe(fds) < 0)
		return -1;
	if (fcntl(fds[0], F_SETFD, FD_CLOEXEC) < 0 || fcntl(fds[1], F_SETFD, FD_CLOEXEC) < 0)
		return -1;
	return 0;
}

int
main(int argc, char **argv)
{
	pid_t		me = getpid();
	pid_t		c;
	int			st;
	int			in[2],
				out[2],
				hold[2];
	char		buf[64];

	if (argc > 1)
		return child(argv[1]);

	CHECK(me >= 100);

	/* a child uppercases through two pipes and exits 7 */
	{
		char	   *cargv[] = {SELF, "upper", NULL};
		int			map[] = {0, 0, 1, 0};

		CHECK(cloexec_pipe(in) == 0 && cloexec_pipe(out) == 0);
		map[1] = in[0];
		map[3] = out[1];
		c = pgmem_spawn(cargv, environ, map, 2);
		CHECK(c > me);
		CHECK(close(in[0]) == 0 && close(out[1]) == 0);
		CHECK(write(in[1], "hello", 5) == 5 && close(in[1]) == 0);
		CHECK(read_all(out[0], buf, sizeof(buf)) == 5 && memcmp(buf, "HELLO", 5) == 0);
		CHECK(close(out[0]) == 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 7);
		CHECK(waitpid(c, &st, 0) == -1 && errno == ECHILD);
		CHECK(kill(c, 0) == -1 && errno == ESRCH);
	}

	/* a trapping child reports SIGABRT */
	{
		char	   *cargv[] = {SELF, "trap", NULL};

		c = pgmem_spawn(cargv, environ, NULL, 0);
		CHECK(c > 0);
		CHECK(waitpid(c, &st, 0) == c && WIFSIGNALED(st) && WTERMSIG(st) == SIGABRT);
	}

	/* WNOHANG and kill(pid, 0) on a running child */
	{
		char	   *cargv[] = {SELF, "hold", NULL};
		int			map[] = {0, 0};

		CHECK(cloexec_pipe(hold) == 0);
		map[1] = hold[0];
		c = pgmem_spawn(cargv, environ, map, 1);
		CHECK(c > 0 && close(hold[0]) == 0);
		CHECK(waitpid(c, &st, WNOHANG) == 0);
		CHECK(kill(c, 0) == 0);
		CHECK(close(hold[1]) == 0);
		CHECK(waitpid(-1, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
	}

	/* the child sees its parent's pid and the environment it was given */
	{
		char	   *cargv[] = {SELF, "info", NULL};
		char	   *cenv[] = {"PGMEM_TEST=hi", NULL};
		int			map[] = {1, 0};
		char		want[64];

		CHECK(cloexec_pipe(out) == 0);
		map[1] = out[1];
		c = pgmem_spawn(cargv, cenv, map, 1);
		CHECK(c > 0 && close(out[1]) == 0);
		memset(buf, 0, sizeof(buf));
		CHECK(read_all(out[0], buf, sizeof(buf) - 1) > 0);
		snprintf(want, sizeof(want), "%d hi", (int) me);
		CHECK(strcmp(buf, want) == 0);
		CHECK(close(out[0]) == 0);
		CHECK(waitpid(c, &st, 0) == c);
	}

	/* a child inherits an fd above 3 (child fd 5 is a pipe end) */
	{
		char	   *cargv[] = {SELF, "fd5", NULL};
		int			map[] = {5, 0};

		CHECK(cloexec_pipe(out) == 0);
		map[1] = out[1];
		c = pgmem_spawn(cargv, environ, map, 1);
		CHECK(c > 0 && close(out[1]) == 0);
		memset(buf, 0, sizeof(buf));
		CHECK(read_all(out[0], buf, sizeof(buf) - 1) == 4 && strcmp(buf, "five") == 0);
		CHECK(close(out[0]) == 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
	}

	/* bad spawns */
	{
		char	   *nope[] = {"/fixtures/nope", NULL};
		char	   *cargv[] = {SELF, "hold", NULL};
		int			map3[] = {3, 0};
		int			mapbad[] = {0, 77};

		CHECK(pgmem_spawn(nope, environ, NULL, 0) == -1 && errno == ENOENT);
		CHECK(pgmem_spawn(cargv, environ, map3, 1) == -1 && errno == EINVAL);
		CHECK(pgmem_spawn(cargv, environ, mapbad, 1) == -1 && errno == EBADF);
		CHECK(wait(&st) == -1 && errno == ECHILD);
	}

	puts("procs ok");
	return 0;
}
