/*
 * Signals under the pgmem kernel. Without arguments it runs the checks and
 * prints "signals ok"; the other modes are children it starts.
 */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define SELF "/fixtures/signals"

extern char **environ;

static volatile int got[32];

static void
count(int signo)
{
	got[signo]++;
}

static void
on(int signo)
{
	struct sigaction sa = {0};

	sa.sa_handler = count;
	sigaction(signo, &sa, NULL);
}

/*
 * The "nest" child's handlers form a chain: each makes host calls and sends
 * itself the next signal, so its dispatch nests inside the one running it,
 * up to the kernel's depth cap of 4. depth and max_depth count handlers on
 * the stack; bad counts anything that went wrong.
 */
static const int chain[] = {SIGHUP, SIGINT, SIGQUIT, SIGUSR1, SIGUSR2, SIGTERM};
#define CHAIN_LEN ((int) (sizeof(chain) / sizeof(chain[0])))
#define DEPTH_CAP 4

static volatile int depth,
			max_depth,
			bad;

static void
link_handler(int signo)
{
	int			next = 0;

	if (++depth > max_depth)
		max_depth = depth;
	for (int i = 0; i + 1 < CHAIN_LEN; i++)
		if (chain[i] == signo)
			next = chain[i + 1];
	if (getppid() <= 0)
		bad++;
	if (next && kill(getpid(), next) != 0)
		bad++;

	/*
	 * At the cap the signal just sent stays pending and is deliverable, yet
	 * a blocking call here must block, not fail with EINTR at once.
	 */
	if (depth == DEPTH_CAP && next)
	{
		struct timespec ts = {.tv_nsec = 1000000};

		if (nanosleep(&ts, NULL) != 0)
			bad++;
	}
	depth--;
}

static pid_t
spawn(char *mode, int map0)
{
	char	   *argv[] = {SELF, mode, NULL};
	int			map[] = {0, map0};

	return pgmem_spawn(argv, environ, map0 >= 0 ? map : NULL, map0 >= 0 ? 1 : 0);
}

static int
child(const char *mode)
{
	char		c;

	if (strcmp(mode, "sleeper") == 0)
	{
		/* blocks in read until killed (default action of SIGTERM) */
		while (read(0, &c, 1) != 0)
			;
		return 0;
	}
	if (strcmp(mode, "catcher") == 0)
	{
		/*
		 * Says it is ready on stdout; read is interrupted by SIGUSR1, then
		 * exits with the count.
		 */
		on(SIGUSR1);
		if (write(1, "r", 1) != 1)
			return 96;
		while (got[SIGUSR1] == 0)
			if (read(0, &c, 1) == 0)
				return 99;
		return 40 + got[SIGUSR1];
	}
	if (strcmp(mode, "storm") == 0)
	{
		/*
		 * Handlers that make host calls while signals keep coming. Signals
		 * of one kind coalesce, so only "at least one ran" is checked; the
		 * point is that nothing crashes or wedges.
		 */
		on(SIGUSR2);
		if (write(1, "r", 1) != 1)
			return 96;
		while (read(0, &c, 1) != 0)
			;
		return got[SIGUSR2] > 0 ? 0 : 98;
	}
	if (strcmp(mode, "nest") == 0)
	{
		/*
		 * Says it is ready on stdout, then reads until EOF while the parent
		 * sends signals; exits 40 + the deepest nesting seen, or 30 if
		 * anything went wrong.
		 */
		struct sigaction sa = {0};

		sa.sa_handler = link_handler;
		for (int i = 0; i < CHAIN_LEN; i++)
			sigaction(chain[i], &sa, NULL);
		if (write(1, "r", 1) != 1)
			return 96;
		while (read(0, &c, 1) != 0)
			;
		return bad ? 30 : 40 + max_depth;
	}
	if (strcmp(mode, "inherit") == 0)
	{
		/*
		 * Starts with SIGUSR1 blocked, as its parent had it, so a SIGUSR1
		 * sent at once waits for the handler; exits 40 + the count.
		 */
		sigset_t	set;

		if (sigprocmask(SIG_BLOCK, NULL, &set) != 0 || sigismember(&set, SIGUSR1) != 1)
			return 95;
		on(SIGUSR1);
		sigemptyset(&set);
		sigaddset(&set, SIGUSR1);
		if (sigprocmask(SIG_UNBLOCK, &set, NULL) != 0)
			return 94;
		while (got[SIGUSR1] == 0)
			pause();
		return 40 + got[SIGUSR1];
	}
	if (strcmp(mode, "itimer") == 0)
	{
		/*
		 * A 20 ms interval timer re-arms itself: waits up to 2 s for 3
		 * SIGALRMs and prints the milliseconds that took.
		 */
		struct itimerval v = {.it_value = {.tv_usec = 20000}, .it_interval = {.tv_usec = 20000}};
		struct itimerval off = {0};
		struct pollfd p = {.fd = -1};
		struct timespec t0,
					t1;
		char		buf[32];
		long		ms;

		on(SIGALRM);
		clock_gettime(CLOCK_MONOTONIC, &t0);
		if (setitimer(ITIMER_REAL, &v, NULL) != 0)
			return 90;
		do
		{
			poll(&p, 0, 2000);
			clock_gettime(CLOCK_MONOTONIC, &t1);
			ms = (t1.tv_sec - t0.tv_sec) * 1000 + (t1.tv_nsec - t0.tv_nsec) / 1000000;
		} while (got[SIGALRM] < 3 && ms < 2000);
		setitimer(ITIMER_REAL, &off, NULL);
		if (got[SIGALRM] < 3)
			return 91;
		write(1, buf, snprintf(buf, sizeof(buf), "%ld\n", ms));
		return 0;
	}
	if (strcmp(mode, "pauser") == 0)
	{
		/*
		 * Says it is ready on stdout, then waits in pause() until a signal's
		 * default action ends it.
		 */
		if (write(1, "r", 1) != 1)
			return 96;
		for (;;)
			pause();
	}
	return 97;
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

/*
 * Spawns a child running mode with stdin the read end of a new pipe, whose
 * write end it stores in *hold, and stdout a pipe; returns once the child
 * has written its ready byte there (its handlers are installed), so no
 * signal reaches it before they are.
 */
static pid_t
spawn_ready(char *mode, int *hold)
{
	char	   *argv[] = {SELF, mode, NULL};
	int			in[2],
				ready[2],
				map[4];
	char		r;
	pid_t		c;

	if (cloexec_pipe(in) < 0 || cloexec_pipe(ready) < 0)
		return -1;
	map[0] = 0;
	map[1] = in[0];
	map[2] = 1;
	map[3] = ready[1];
	c = pgmem_spawn(argv, environ, map, 2);
	close(in[0]);
	close(ready[1]);
	if (c > 0 && read(ready[0], &r, 1) != 1)
		c = -1;
	close(ready[0]);
	*hold = in[1];
	return c;
}

int
main(int argc, char **argv)
{
	int			st;
	int			hold[2];
	pid_t		c;

	if (argc > 1)
		return child(argv[1]);

	/* raise runs a handler at once; a blocked signal waits for unblock */
	{
		sigset_t	set,
					old;

		on(SIGUSR1);
		CHECK(raise(SIGUSR1) == 0 && got[SIGUSR1] == 1);
		sigemptyset(&set);
		sigaddset(&set, SIGUSR1);
		CHECK(sigprocmask(SIG_BLOCK, &set, &old) == 0);
		CHECK(kill(getpid(), SIGUSR1) == 0);
		CHECK(got[SIGUSR1] == 1);
		CHECK(sigprocmask(SIG_SETMASK, &old, NULL) == 0);
		CHECK(got[SIGUSR1] == 2);

		/* kill(0) sends to the caller's process group: itself */
		CHECK(kill(0, SIGUSR1) == 0 && got[SIGUSR1] == 3);
	}

	/* an ignored signal neither runs anything nor interrupts */
	{
		struct sigaction sa = {0};

		sa.sa_handler = SIG_IGN;
		CHECK(sigaction(SIGUSR2, &sa, NULL) == 0);
		CHECK(kill(getpid(), SIGUSR2) == 0 && got[SIGUSR2] == 0);
	}

	/* SIGALRM from setitimer interrupts poll with EINTR */
	{
		struct itimerval v = {.it_value = {.tv_usec = 30000}};
		struct pollfd p = {.fd = -1};

		on(SIGALRM);
		CHECK(setitimer(ITIMER_REAL, &v, NULL) == 0);
		CHECK(poll(&p, 0, 5000) == -1 && errno == EINTR);
		CHECK(got[SIGALRM] == 1);
	}

	/* nanosleep is interrupted too */
	{
		struct itimerval v = {.it_value = {.tv_usec = 20000}};
		struct timespec ts = {.tv_sec = 5};

		CHECK(setitimer(ITIMER_REAL, &v, NULL) == 0);
		CHECK(nanosleep(&ts, NULL) == -1 && errno == EINTR && got[SIGALRM] == 2);
	}

	/* a child blocked in read: its handler runs, read returns EINTR */
	c = spawn_ready("catcher", &hold[1]);
	CHECK(c > 0);
	CHECK(kill(c, SIGUSR1) == 0);
	CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 41);
	CHECK(close(hold[1]) == 0);

	/* default action of SIGTERM kills a blocked child; kill(-pid) works */
	CHECK(cloexec_pipe(hold) == 0);
	c = spawn("sleeper", hold[0]);
	CHECK(c > 0 && close(hold[0]) == 0);
	CHECK(kill(-c, SIGTERM) == 0);
	CHECK(waitpid(c, &st, 0) == c && WIFSIGNALED(st) && WTERMSIG(st) == SIGTERM);
	CHECK(close(hold[1]) == 0);

	/* waitpid itself is interrupted by a signal */
	{
		struct itimerval v = {.it_value = {.tv_usec = 20000}};

		CHECK(cloexec_pipe(hold) == 0);
		c = spawn("sleeper", hold[0]);
		CHECK(c > 0 && close(hold[0]) == 0);
		CHECK(setitimer(ITIMER_REAL, &v, NULL) == 0);
		CHECK(waitpid(c, &st, 0) == -1 && errno == EINTR);
		CHECK(kill(c, SIGKILL) == 0);
		CHECK(waitpid(c, &st, 0) == c && WIFSIGNALED(st) && WTERMSIG(st) == SIGKILL);
		CHECK(close(hold[1]) == 0);
	}

	/* a storm of signals at a child whose handler keeps making host calls */
	c = spawn_ready("storm", &hold[1]);
	CHECK(c > 0);
	for (int i = 0; i < 400; i++)
		kill(c, SIGUSR2);
	CHECK(close(hold[1]) == 0);	/* EOF ends the child's loop */
	CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);

	/*
	 * Nested dispatch: handlers that make host calls and signal each other
	 * nest exactly to the depth cap, while the parent keeps signalling.
	 */
	{
		c = spawn_ready("nest", &hold[1]);
		CHECK(c > 0);
		for (int i = 0; i < 200; i++)
			kill(c, i % 2 ? SIGUSR1 : SIGHUP);
		CHECK(close(hold[1]) == 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 40 + DEPTH_CAP);
	}

	/*
	 * A child's exit sends its parent SIGCHLD before the parent can reap
	 * it, so the handler has run when waitpid returns, and the signal does
	 * not make waitpid fail with EINTR.
	 */
	{
		on(SIGCHLD);
		CHECK(cloexec_pipe(hold) == 0);
		c = spawn("sleeper", hold[0]);
		CHECK(c > 0 && close(hold[0]) == 0);
		CHECK(close(hold[1]) == 0);	/* EOF: the sleeper returns 0 */
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0);
		CHECK(got[SIGCHLD] == 1);
		CHECK(signal(SIGCHLD, SIG_DFL) != SIG_ERR);
	}

	/*
	 * A child starts with its parent's signal mask, as Postgres's children
	 * start with every signal blocked: a signal sent at once waits for the
	 * child's handler instead of killing it.
	 */
	{
		sigset_t	set,
					old;

		sigemptyset(&set);
		sigaddset(&set, SIGUSR1);
		CHECK(sigprocmask(SIG_BLOCK, &set, &old) == 0);
		c = spawn("inherit", -1);
		CHECK(c > 0 && kill(c, SIGUSR1) == 0);
		CHECK(sigprocmask(SIG_SETMASK, &old, NULL) == 0);
		CHECK(waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 41);
	}

	/* process groups */
	CHECK(setsid() == getpid() && getpgrp() == getpid());

	puts("signals ok");
	return 0;
}
