/*
 * System V shared memory under the pgmem kernel. Without arguments it runs
 * the checks and prints "shm ok"; child-* are children it starts, and
 * "holder" and "holder-spin" are started from Go.
 */
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/shm.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define SELF "/fixtures/shm"
#define WINDOW ((void *) 0x20000000)	/* PG_SHMEM_ADDR: 512 MiB */
#define SEG (1 << 20)
#define INCS 200000

extern char **environ;

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
nattch(int id)
{
	struct shmid_ds ds;

	if (shmctl(id, IPC_STAT, &ds) < 0)
		return -1;
	return (long) ds.shm_nattch;
}

/* child-inc ID: adds 1 to the counter at WINDOW, INCS times. */
static int
child_inc(int id)
{
	volatile uint32_t *p = shmat(id, WINDOW, 0);

	CHECK(p == WINDOW);
	for (int i = 0; i < INCS; i++)
		__atomic_fetch_add(p, 1, __ATOMIC_SEQ_CST);
	CHECK(shmdt((void *) p) == 0);
	return 0;
}

/* child-dyn ID: attaches wherever shmat likes and leaves a message. */
static int
child_dyn(int id)
{
	char	   *p = shmat(id, NULL, 0);

	CHECK(p != (void *) -1);
	strcpy(p + 64, "hello from child");
	CHECK(shmdt(p) == 0);
	return 0;
}

/* child-keep ID: attaches and exits without detaching. */
static int
child_keep(int id)
{
	CHECK(shmat(id, NULL, 0) != (void *) -1);
	return 0;
}

/*
 * holder: attaches a new segment, marks it removed, writes "r" to stdout
 * and waits to be killed: in pause, or (spin) computing without host calls,
 * so only preemption can stop it.
 */
static int
holder(int spin)
{
	int			id = shmget(IPC_PRIVATE, SEG, IPC_CREAT | 0600);
	char	   *p;

	CHECK(id > 0);
	p = shmat(id, NULL, 0);
	CHECK(p != (void *) -1);
	memset(p, 1, SEG);
	CHECK(shmctl(id, IPC_RMID, NULL) == 0);
	CHECK(write(1, "r", 1) == 1);
	if (spin)
	{
		volatile uint32_t *n = (volatile uint32_t *) p;

		for (;;)
			(*n)++;
	}
	for (;;)
		pause();
}

static int
keys(void)
{
	struct shmid_ds ds;
	int			a = shmget(IPC_PRIVATE, 4096, IPC_CREAT | 0600),
				b = shmget(IPC_PRIVATE, 4096, IPC_CREAT | 0600),
				k = shmget(5432001, 8192, IPC_CREAT | IPC_EXCL | 0600);

	CHECK(a > 0 && b > 0 && a != b && k > 0);
	CHECK(shmget(5432001, 8192, IPC_CREAT | IPC_EXCL | 0600) < 0 && errno == EEXIST);
	CHECK(shmget(5432001, 4096, 0) == k);
	CHECK(shmget(5432001, 0, 0600) == k);	/* dsm_impl.c's attach */
	CHECK(shmget(5432001, 16384, 0) < 0 && errno == EINVAL);
	CHECK(shmget(5432002, 4096, 0) < 0 && errno == ENOENT);
	CHECK(shmget(IPC_PRIVATE, 0, IPC_CREAT) < 0 && errno == EINVAL);
	CHECK(shmctl(a, IPC_RMID, NULL) == 0 && shmctl(b, IPC_RMID, NULL) == 0 && shmctl(k, IPC_RMID, NULL) == 0);
	CHECK(shmget(5432001, 4096, 0) < 0 && errno == ENOENT);
	CHECK(shmctl(k, IPC_STAT, &ds) < 0 && errno == EINVAL);
	return 0;
}

static int
heap(void)
{
	struct shmid_ds ds;
	int			id = shmget(IPC_PRIVATE, 100000, IPC_CREAT | 0600);
	unsigned char *p,
			   *q;
	void	   *m;

	CHECK(id > 0);
	p = shmat(id, NULL, 0);
	CHECK(p != (void *) -1 && (uintptr_t) p % 65536 == 0);
	for (int i = 0; i < 100000; i++)
		CHECK(p[i] == 0);
	memset(p, 0xab, 100000);
	q = shmat(id, NULL, 0);
	CHECK(q != (void *) -1 && q != p && q[99999] == 0xab);
	q[0] = 7;
	CHECK(p[0] == 7);
	CHECK(shmctl(id, IPC_STAT, &ds) == 0);
	CHECK(ds.shm_nattch == 2 && ds.shm_segsz == 100000 && ds.shm_perm.uid == geteuid() && ds.shm_perm.mode == 0600);
	CHECK(shmdt(q) == 0 && nattch(id) == 1);
	CHECK(shmdt(p) == 0 && nattch(id) == 0);
	CHECK(shmdt(p) < 0 && errno == EINVAL);
	m = malloc(1 << 20);
	CHECK(m != NULL);
	memset(m, 1, 1 << 20);
	free(m);
	CHECK(shmctl(id, IPC_RMID, NULL) == 0);
	return 0;
}

static int
window(void)
{
	struct shmid_ds ds;
	int			id = shmget(IPC_PRIVATE, SEG, IPC_CREAT | 0600);
	volatile uint32_t *p;

	CHECK(id > 0);
	p = shmat(id, WINDOW, 0);
	CHECK(p == WINDOW);
	CHECK((uint64_t) __builtin_wasm_memory_size(0) * 65536 >= 0x20000000ull + SEG);
	p[1] = 99;
	/* The heap continues above the window and never overlaps it. */
	for (int i = 0; i < 8; i++)
	{
		char	   *m = malloc(1 << 20);

		CHECK(m != NULL);
		CHECK((uintptr_t) m + (1 << 20) <= 0x20000000u || (uintptr_t) m >= 0x20000000u + SEG);
		memset(m, 2, 1 << 20);
	}
	CHECK(p[1] == 99);
	CHECK(shmat(id, (void *) 0x10000, 0) == (void *) -1 && errno == EINVAL);	/* the heap's */
	CHECK(shmat(id, (char *) WINDOW + 1, 0) == (void *) -1 && errno == EINVAL);
	CHECK(shmat(id, WINDOW, 0) == (void *) -1 && errno == EINVAL);	/* taken */

	/* Two processes count at the same address with exact atomics. */
	p[0] = 0;
	{
		pid_t		c1 = spawn("child-inc", id),
					c2 = spawn("child-inc", id);

		CHECK(exited_ok(c1) && exited_ok(c2));
	}
	CHECK(p[0] == 2 * INCS);

	/* A child attached elsewhere writes what the parent reads here. */
	CHECK(exited_ok(spawn("child-dyn", id)));
	CHECK(strcmp((char *) p + 64, "hello from child") == 0);

	/* A child's attachment ends with it. */
	CHECK(exited_ok(spawn("child-keep", id)));
	CHECK(nattch(id) == 1);

	/* Detach and attach again in the reserved range: the data is kept. */
	CHECK(shmdt((void *) p) == 0);
	CHECK(shmat(id, WINDOW, 0) == WINDOW);
	CHECK(p[0] == 2 * INCS);

	/* Removed but attached: usable until the last detach frees it. */
	CHECK(shmctl(id, IPC_RMID, NULL) == 0);
	p[2] = 3;
	CHECK(nattch(id) == 1);
	CHECK(shmdt((void *) p) == 0);
	CHECK(shmctl(id, IPC_STAT, &ds) < 0 && errno == EINVAL);
	return 0;
}

int
main(int argc, char **argv)
{
	if (argc == 3 && strcmp(argv[1], "child-inc") == 0)
		return child_inc(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-dyn") == 0)
		return child_dyn(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-keep") == 0)
		return child_keep(atoi(argv[2]));
	if (argc == 2 && strcmp(argv[1], "holder") == 0)
		return holder(0);
	if (argc == 2 && strcmp(argv[1], "holder-spin") == 0)
		return holder(1);
	if (keys() || heap() || window())
		return 1;
	printf("shm ok\n");
	return 0;
}
