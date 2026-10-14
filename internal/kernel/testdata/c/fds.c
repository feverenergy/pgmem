/*
 * fd inheritance across pgmem_spawn: same numbers for fds that are not
 * close-on-exec, none of those that are, and the parent's working
 * directory, also in a child that calls no process function (bare). Prints
 * "fds ok". Also: close-on-exec on fd 1, a failed spawn leaking no fd, and
 * a spawn from a deleted working directory (ENOENT).
 */
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define SELF "/fixtures/fds"

extern char **environ;

static int
child(int argc, char **argv)
{
	char		cwd[256];

	(void) argc;
	if (strcmp(argv[1], "probe") == 0)
	{
		/* fd 20 inherited and writable; fd 21 must be closed */
		CHECK(write(20, "inherited", 9) == 9);
		CHECK(write(21, "x", 1) == -1 && errno == EBADF);
		CHECK(getcwd(cwd, sizeof(cwd)) != NULL && strcmp(cwd, "/work/sub") == 0);
		CHECK(access("marker", F_OK) == 0);	/* relative to the inherited cwd */
		return 0;
	}
	if (strcmp(argv[1], "nostdout") == 0)
	{
		/* the parent marked fd 1 close-on-exec: it must not exist here */
		CHECK(write(1, "x", 1) == -1 && errno == EBADF);
		CHECK(fcntl(1, F_GETFD) == -1 && errno == EBADF);
		return 0;
	}
	return 97;
}

int
main(int argc, char **argv)
{
	int			a[2],
				b[2],
				st;
	char		buf[32];

	if (argc > 1)
		return child(argc, argv);

	CHECK(mkdir("/work", 0700) == 0 && mkdir("/work/sub", 0700) == 0);
	CHECK(close(open("/work/sub/marker", O_CREAT | O_WRONLY, 0600)) == 0);
	CHECK(chdir("/work/sub") == 0);

	/* dup2 to fixed numbers; fd 21 close-on-exec */
	CHECK(pipe(a) == 0 && pipe(b) == 0);
	CHECK(dup2(a[1], 20) == 20 && close(a[1]) == 0);
	CHECK(dup2(b[1], 21) == 21 && close(b[1]) == 0);
	CHECK(fcntl(21, F_SETFD, FD_CLOEXEC) == 0);
	CHECK(fcntl(21, F_GETFD) == FD_CLOEXEC && fcntl(20, F_GETFD) == 0);
	CHECK(fcntl(a[0], F_SETFD, FD_CLOEXEC) == 0 && fcntl(b[0], F_SETFD, FD_CLOEXEC) == 0);

	{
		char	   *cargv[] = {SELF, "probe", NULL};
		pid_t		c = pgmem_spawn(cargv, environ, NULL, 0);

		CHECK(c > 0);
		CHECK(close(20) == 0 && close(21) == 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
	}
	CHECK(read(a[0], buf, sizeof(buf)) == 9 && memcmp(buf, "inherited", 9) == 0);

	/* a program that calls no process function still starts in our cwd */
	{
		char	   *cargv[] = {"/fixtures/bare", "cwd", NULL};
		pid_t		c = pgmem_spawn(cargv, environ, NULL, 0);

		CHECK(c > 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
	}
	CHECK(read(b[0], buf, sizeof(buf)) == 0);	/* nobody held b's write end */

	/* F_DUPFD_CLOEXEC and plain dup */
	{
		int			d = fcntl(a[0], F_DUPFD_CLOEXEC, 30);
		int			e = dup(a[0]);

		CHECK(d >= 30 && fcntl(d, F_GETFD) == FD_CLOEXEC);
		CHECK(e >= 0 && e != a[0] && fcntl(e, F_GETFD) == 0);
		CHECK(fcntl(999, F_GETFD) == -1 && errno == EBADF);
	}

	/* close-on-exec on a standard fd: the child has no such fd */
	{
		char	   *cargv[] = {SELF, "nostdout", NULL};
		pid_t		c;

		CHECK(fcntl(1, F_SETFD, FD_CLOEXEC) == 0);
		c = pgmem_spawn(cargv, environ, NULL, 0);
		CHECK(fcntl(1, F_SETFD, 0) == 0);
		CHECK(c > 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
	}

	/*
	 * A spawn that fails after duplicating some of the parent's fds (the
	 * first map entry dups a pipe read end, the second names a bad fd)
	 * returns EBADF and releases the duplicates: with the parent's read end
	 * closed, a write to the pipe sees EPIPE only if no copy leaked.
	 */
	{
		char	   *cargv[] = {SELF, "probe", NULL};
		int			p[2],
					map[4];

		signal(SIGPIPE, SIG_IGN);
		CHECK(pipe(p) == 0);
		map[0] = 20;
		map[1] = p[0];
		map[2] = 21;
		map[3] = 999;
		CHECK(pgmem_spawn(cargv, environ, map, 2) == -1 && errno == EBADF);
		CHECK(close(p[0]) == 0);
		CHECK(write(p[1], "x", 1) == -1 && errno == EPIPE);
		CHECK(close(p[1]) == 0);
	}

	/* sanity: an unregistered path is ENOENT (before any fd is duplicated) */
	{
		char	   *cargv[] = {"/fixtures/no-such-program", NULL};

		CHECK(pgmem_spawn(cargv, environ, NULL, 0) == -1 && errno == ENOENT);
	}

	/*
	 * A working directory that has gone fails the spawn with ENOENT and
	 * starts no child (the kernel checks it before duplicating any fd).
	 */
	{
		char	   *cargv[] = {SELF, "rootcwd", NULL};

		CHECK(mkdir("/work/gone", 0700) == 0 && chdir("/work/gone") == 0);
		CHECK(rmdir("/work/gone") == 0);
		CHECK(pgmem_spawn(cargv, environ, NULL, 0) == -1 && errno == ENOENT);
		CHECK(waitpid(-1, &st, 0) == -1 && errno == ECHILD);
		CHECK(chdir("/work/sub") == 0);
	}

	puts("fds ok");
	return 0;
}
