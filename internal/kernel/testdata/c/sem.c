/*
 * POSIX semaphores under the pgmem kernel. Without arguments it runs the
 * checks and prints "sem ok"; child-* are children it starts, and
 * "blocker" and "keeper" are started from Go.
 */
#include <errno.h>
#include <limits.h>
#include <semaphore.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/shm.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define SELF "/fixtures/sem"
#define WINDOW ((void *) 0x20000000)
#define ROUNDS 2000

extern char **environ;

struct shared
{
	sem_t		ping,
				pong,
				gate;
	volatile int value;
	uintptr_t	parent_addr;	/* where the parent attached it */
};

/* At the same address in every process running this program. */
static sem_t private_sem;

static volatile int usr1;

static void
on_usr1(int signo)
{
	(void) signo;
	usr1++;
}

static struct shared *
attach(int id)
{
	struct shared *s = shmat(id, WINDOW, 0);

	return s == (void *) -1 ? NULL : s;
}

static pid_t
spawn(char *mode, int id)
{
	char		arg[16];
	char	   *argv[] = {SELF, mode, arg, NULL};

	snprintf(arg, sizeof(arg), "%d", id);
	return pgmem_spawn(argv, environ, NULL, 0);
}

static int
exited_ok(pid_t c)
{
	int			st;

	return c > 0 && waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0;
}

static long
ms_between(struct timespec a, struct timespec b)
{
	return (long) (b.tv_sec - a.tv_sec) * 1000 + (b.tv_nsec - a.tv_nsec) / 1000000;
}

/* child-gate ID: waits at the gate, then sets value and posts pong. */
static int
child_gate(int id)
{
	struct shared *s = attach(id);

	CHECK(s != NULL);
	CHECK(sem_wait(&s->gate) == 0);
	s->value = 42;
	CHECK(sem_post(&s->pong) == 0);
	return 0;
}

/* child-pong ID: answers ROUNDS pings. */
static int
child_pong(int id)
{
	struct shared *s = attach(id);

	CHECK(s != NULL);
	for (int i = 0; i < ROUNDS; i++)
	{
		CHECK(sem_wait(&s->ping) == 0);
		s->value++;
		CHECK(sem_post(&s->pong) == 0);
	}
	return 0;
}

/*
 * child-intr ID: SIGUSR1 interrupts its wait at the gate with EINTR, after
 * the handler ran. The parent keeps signalling until it exits, so a signal
 * that lands before sem_wait blocks does no harm.
 */
static int
child_intr(int id)
{
	struct sigaction sa = {0};
	struct shared *s;

	sa.sa_handler = on_usr1;
	CHECK(sigaction(SIGUSR1, &sa, NULL) == 0);
	s = attach(id);
	CHECK(s != NULL);
	CHECK(sem_post(&s->pong) == 0);
	errno = 0;
	CHECK(sem_wait(&s->gate) < 0 && errno == EINTR && usr1 >= 1);
	return 0;
}

/*
 * child-heap ID: attaches in its heap at an address other than the
 * parent's and posts pong there: the same semaphore, keyed by its offset
 * in the segment.
 */
static int
child_heap(int id)
{
	struct shared *s = shmat(id, NULL, 0);

	CHECK(s != (void *) -1);
	if ((uintptr_t) s == s->parent_addr)
		s = shmat(id, NULL, 0);	/* the first stays attached */
	CHECK(s != (void *) -1 && (uintptr_t) s != s->parent_addr);
	CHECK(sem_post(&s->pong) == 0);
	return 0;
}

/* child-private: its private_sem is its own, not the parent's. */
static int
child_private(void)
{
	CHECK(sem_init(&private_sem, 0, 0) == 0);
	CHECK(sem_trywait(&private_sem) < 0 && errno == EAGAIN);
	CHECK(sem_destroy(&private_sem) == 0);
	return 0;
}

/*
 * keeper: creates a keyed segment it never removes, attaches it at the
 * window with a semaphore in it, writes "r" to stdout and waits to be
 * killed (Postgres's postmaster and its main segment at Kernel.Close).
 */
static int
keeper(void)
{
	int			id = shmget(5432100, sizeof(struct shared), IPC_CREAT | 0600);
	struct shared *s;

	CHECK(id > 0);
	s = attach(id);
	CHECK(s != NULL);
	CHECK(sem_init(&s->gate, 1, 0) == 0);
	CHECK(write(1, "r", 1) == 1);
	for (;;)
		pause();
}

/* blocker: writes "r" to stdout and waits forever; Go kills it. */
static int
blocker(void)
{
	CHECK(sem_init(&private_sem, 0, 0) == 0);
	CHECK(write(1, "r", 1) == 1);
	sem_wait(&private_sem);
	printf("FAIL blocker woke\n");
	return 1;
}

static int
local(void)
{
	struct timespec t0,
				t1,
				at;
	int			v;

	CHECK(sem_init(&private_sem, 0, 1) == 0);
	CHECK(sem_trywait(&private_sem) == 0);
	CHECK(sem_trywait(&private_sem) < 0 && errno == EAGAIN);
	CHECK(sem_post(&private_sem) == 0 && sem_getvalue(&private_sem, &v) == 0 && v == 1);
	CHECK(sem_wait(&private_sem) == 0);

	clock_gettime(CLOCK_REALTIME, &t0);
	at = t0;
	at.tv_nsec += 50 * 1000000;
	if (at.tv_nsec >= 1000000000)
	{
		at.tv_sec++;
		at.tv_nsec -= 1000000000;
	}
	CHECK(sem_timedwait(&private_sem, &at) < 0 && errno == ETIMEDOUT);
	clock_gettime(CLOCK_REALTIME, &t1);
	CHECK(ms_between(t0, t1) >= 40);
	at.tv_nsec = 1000000000;
	CHECK(sem_timedwait(&private_sem, &at) < 0 && errno == EINVAL);

	/* Same address in another process: a different semaphore. */
	CHECK(sem_init(&private_sem, 0, 5) == 0);
	{
		char	   *argv[] = {SELF, "child-private", NULL};

		CHECK(exited_ok(pgmem_spawn(argv, environ, NULL, 0)));
	}
	CHECK(sem_getvalue(&private_sem, &v) == 0 && v == 5);

	CHECK(sem_init(&private_sem, 0, SEM_VALUE_MAX) == 0);
	CHECK(sem_post(&private_sem) < 0 && errno == EOVERFLOW);
	CHECK(sem_destroy(&private_sem) == 0);
	CHECK(sem_post(&private_sem) < 0 && errno == EINVAL);
	CHECK(sem_init(&private_sem, 0, (unsigned) SEM_VALUE_MAX + 1) < 0 && errno == EINVAL);
	return 0;
}

static int
shared(void)
{
	int			id = shmget(IPC_PRIVATE, sizeof(struct shared), IPC_CREAT | 0600);
	struct shared *s;
	pid_t		c;
	int			st,
				tries;
	struct timespec t0,
				t1,
				at;

	CHECK(id > 0);
	s = attach(id);
	CHECK(s != NULL);
	CHECK(sem_init(&s->ping, 1, 0) == 0 && sem_init(&s->pong, 1, 0) == 0 && sem_init(&s->gate, 1, 0) == 0);

	/* A child blocks at the gate until the parent opens it. */
	c = spawn("child-gate", id);
	CHECK(c > 0);
	usleep(20000);
	CHECK(s->value == 0);
	CHECK(sem_post(&s->gate) == 0 && sem_wait(&s->pong) == 0 && s->value == 42);
	CHECK(exited_ok(c));

	/* A post wakes sem_timedwait long before its deadline. */
	s->value = 0;
	c = spawn("child-gate", id);
	CHECK(c > 0);
	clock_gettime(CLOCK_REALTIME, &t0);
	at = t0;
	at.tv_sec += 5;
	CHECK(sem_post(&s->gate) == 0 && sem_timedwait(&s->pong, &at) == 0 && s->value == 42);
	clock_gettime(CLOCK_REALTIME, &t1);
	CHECK(ms_between(t0, t1) < 4000);
	CHECK(exited_ok(c));

	/* Ping-pong across processes. */
	s->value = 0;
	c = spawn("child-pong", id);
	CHECK(c > 0);
	for (int i = 0; i < ROUNDS; i++)
		CHECK(sem_post(&s->ping) == 0 && sem_wait(&s->pong) == 0);
	CHECK(s->value == ROUNDS && exited_ok(c));

	/* A signal interrupts a blocked sem_wait. */
	c = spawn("child-intr", id);
	CHECK(c > 0 && sem_wait(&s->pong) == 0);
	for (tries = 0; tries < 1000; tries++)
	{
		if (waitpid(c, &st, WNOHANG) == c)
			break;
		kill(c, SIGUSR1);
		usleep(5000);
	}
	CHECK(tries < 1000 && WIFEXITED(st) && WEXITSTATUS(st) == 0);

	/* The segment's semaphores go with it. */
	CHECK(shmdt(s) == 0 && shmctl(id, IPC_RMID, NULL) == 0);
	return 0;
}

/* A semaphore in a segment attached at different heap addresses. */
static int
heap_shared(void)
{
	int			id = shmget(IPC_PRIVATE, sizeof(struct shared), IPC_CREAT | 0600);
	struct shared *s;

	CHECK(id > 0);
	s = shmat(id, NULL, 0);
	CHECK(s != (void *) -1 && s != WINDOW);
	s->parent_addr = (uintptr_t) s;
	CHECK(sem_init(&s->pong, 1, 0) == 0);
	CHECK(exited_ok(spawn("child-heap", id)));
	CHECK(sem_wait(&s->pong) == 0);
	CHECK(sem_trywait(&s->pong) < 0 && errno == EAGAIN);
	CHECK(shmdt(s) == 0 && shmctl(id, IPC_RMID, NULL) == 0);
	return 0;
}

int
main(int argc, char **argv)
{
	if (argc == 3 && strcmp(argv[1], "child-gate") == 0)
		return child_gate(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-pong") == 0)
		return child_pong(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-intr") == 0)
		return child_intr(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-heap") == 0)
		return child_heap(atoi(argv[2]));
	if (argc == 2 && strcmp(argv[1], "child-private") == 0)
		return child_private();
	if (argc == 2 && strcmp(argv[1], "blocker") == 0)
		return blocker();
	if (argc == 2 && strcmp(argv[1], "keeper") == 0)
		return keeper();
	if (local() || shared() || heap_shared())
		return 1;
	printf("sem ok\n");
	return 0;
}
