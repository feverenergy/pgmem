/*
 * Processes, implemented by the pgmem kernel through the "pgm" host module.
 * Host calls return a negative WASI errno on failure; wasi-libc's errno
 * values are the same numbers.
 */
#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("getpid") int32_t pgm_getpid(void);
PGM_IMPORT("getppid") int32_t pgm_getppid(void);
PGM_IMPORT("pipe") int32_t pgm_pipe(int32_t *fds);
PGM_IMPORT("spawn") int32_t pgm_spawn(const char *argv, int32_t argv_len,
									  const char *envp, int32_t envp_len,
									  const char *cwd, int32_t cwd_len,
									  const int32_t *fdmap, int32_t fdmap_count);
PGM_IMPORT("wait") int32_t pgm_wait(int32_t pid, int32_t *status, int32_t options);
PGM_IMPORT("kill") int32_t pgm_kill(int32_t pid, int32_t sig);
PGM_IMPORT("pause") int32_t pgm_pause(void);
PGM_IMPORT("startup_cwd") int32_t pgm_startup_cwd(char *buf, int32_t len);

static int
host_result(int32_t r)
{
	if (r < 0)
	{
		errno = -r;
		return -1;
	}
	return r;
}

pid_t
getpid(void)
{
	return pgm_getpid();
}

pid_t
getppid(void)
{
	return pgm_getppid();
}

int
pipe(int fds[2])
{
	int32_t		pair[2];

	if (host_result(pgm_pipe(pair)) < 0)
		return -1;
	fds[0] = pair[0];
	fds[1] = pair[1];
	return 0;
}

pid_t
waitpid(pid_t pid, int *status, int options)
{
	int32_t		st = 0;
	int32_t		r = pgm_wait(pid, &st, options);

	if (r > 0 && status)
		*status = st;
	return host_result(r);
}

pid_t
wait(int *status)
{
	return waitpid(-1, status, 0);
}

/*
 * A negative pid names a process group; every process leads its own, so the
 * kernel sends to process -pid. A signal sent to this process runs its
 * handler before kill returns.
 */
int
kill(pid_t pid, int signo)
{
	return host_result(pgm_kill(pid, signo));
}

/* Waits until a signal is delivered; always fails with EINTR. */
int
pause(void)
{
	return host_result(pgm_pause());
}

/*
 * Every process is the leader of its own session and process group, so
 * these change nothing.
 */
pid_t
setsid(void)
{
	return getpid();
}

pid_t
getpgrp(void)
{
	return getpid();
}

int
setpgid(pid_t pid, pid_t pgid)
{
	(void) pid;
	(void) pgid;
	return 0;
}

/* Packs a NULL-terminated string vector as NUL-separated bytes. */
static char *
pack(char *const vec[], int32_t *len)
{
	size_t		n = 0;
	char	   *buf,
			   *p;

	for (int i = 0; vec && vec[i]; i++)
		n += strlen(vec[i]) + 1;
	buf = malloc(n ? n : 1);
	if (!buf)
		return NULL;
	p = buf;
	for (int i = 0; vec && vec[i]; i++)
	{
		size_t		k = strlen(vec[i]) + 1;

		memcpy(p, vec[i], k);
		p += k;
	}
	*len = (int32_t) n;
	return buf;
}

/*
 * The child starts in our working directory, which the kernel holds for its
 * startup_cwd; it inherits the fds the kernel's fd table says are not
 * close-on-exec.
 */
pid_t
pgmem_spawn(char *const argv[], char *const envp[], const int *fdmap, int nfdmap)
{
	int32_t		argv_len,
				envp_len;
	char		cwd[PATH_MAX];
	char	   *a,
			   *e;
	int32_t		r;

	if (getcwd(cwd, sizeof(cwd)) == NULL)
		return -1;
	a = pack(argv, &argv_len);
	e = pack(envp, &envp_len);
	if (!a || !e)
	{
		free(a);
		free(e);
		errno = ENOMEM;
		return -1;
	}
	r = pgm_spawn(a, argv_len, e, envp_len, cwd, (int32_t) strlen(cwd),
				  (const int32_t *) fdmap, nfdmap);
	free(a);
	free(e);
	return host_result(r);
}

/*
 * A spawned child moves to its parent's working directory before main
 * runs. Priority 65535 runs this after wasi-libc's prioritized constructors
 * (its environment setup); wasi-libc reads the preopens that chdir resolves
 * paths against lazily, on the first path lookup, so chdir works here. A
 * directory that has gone since the parent spawned leaves the child at "/",
 * as it would a process started from Go. It is not static so that abi.c
 * can anchor it into programs that call nothing else in this file.
 */
void		pgmem_startup_chdir(void);

__attribute__((constructor(65535)))
void
pgmem_startup_chdir(void)
{
	char		cwd[PATH_MAX];
	int32_t		n = pgm_startup_cwd(cwd, sizeof(cwd) - 1);

	if (n <= 0)
		return;
	cwd[n] = '\0';
	(void) chdir(cwd);
}
