/* Checks the compat layer from inside WebAssembly. Prints "compat ok". */
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <netdb.h>
#include <pwd.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/shm.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <unistd.h>

_Static_assert(sizeof(sigset_t) == 8, "sigset_t must hold 64 signals");

static int got;

static void
on_usr1(int signo)
{
	got = signo;
}

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s\n", __FILE__, __LINE__, #cond); return 1; } } while (0)

int
main(void)
{
	sigset_t	set, old;
	struct sigaction act = {0}, prev;
	struct rlimit rl;
	struct passwd *pw;
	jmp_buf		jb;
	volatile int jumped = 0;

	CHECK(sigemptyset(&set) == 0);
	CHECK(sigaddset(&set, SIGALRM) == 0 && sigaddset(&set, 31) == 0);
	CHECK(sigismember(&set, SIGALRM) == 1 && sigismember(&set, 31) == 1);
	CHECK(sigismember(&set, SIGUSR1) == 0);
	CHECK(sigprocmask(SIG_BLOCK, &set, &old) == 0 && old == 0);
	CHECK(sigprocmask(SIG_SETMASK, NULL, &old) == 0 && old == set);

	act.sa_handler = on_usr1;
	CHECK(sigaction(SIGUSR1, &act, &prev) == 0 && prev.sa_handler == SIG_DFL);
	CHECK(raise(SIGUSR1) == 0 && got == SIGUSR1);
	CHECK(sigaction(SIGKILL, &act, NULL) == -1 && errno == EINVAL);

	/* volatile stops the compiler from folding distinct-address comparisons */
	{
		void		(*volatile ign) (int) = SIG_IGN;
		void		(*volatile err) (int) = SIG_ERR;
		void		(*volatile dfl) (int) = SIG_DFL;

		CHECK(ign != err && ign != dfl && err != dfl);
	}
	CHECK(signal(SIGPIPE, SIG_IGN) == SIG_DFL);
	CHECK(raise(SIGPIPE) == 0);
	CHECK(signal(SIGPIPE, SIG_DFL) == SIG_IGN);

	/*
	 * A pending bit that is also ignored (a signal that raced
	 * sigaction(SIG_IGN)) is not deliverable: dispatch never clears it, so
	 * CHECK_FOR_INTERRUPTS would otherwise dispatch every time.
	 */
	{
		uint64_t	bit = (uint64_t) 1 << (SIGUSR2 - 1);

		CHECK(!PGMEM_SIGNALS_DELIVERABLE());
		CHECK(signal(SIGUSR2, SIG_IGN) == SIG_DFL);
		__atomic_fetch_or(&pgmem_sigstate.pending, bit, __ATOMIC_SEQ_CST);
		CHECK(!PGMEM_SIGNALS_DELIVERABLE());
		__atomic_fetch_and(&pgmem_sigstate.pending, ~bit, __ATOMIC_SEQ_CST);
		CHECK(signal(SIGUSR2, SIG_DFL) == SIG_IGN);
	}

	CHECK(geteuid() == 1000 && geteuid() != 0);
	pw = getpwuid(geteuid());
	CHECK(pw != NULL && pw->pw_name[0] == 'p');
	CHECK(umask(077) == 022 && umask(022) == 077);
	CHECK(getrlimit(RLIMIT_STACK, &rl) == 0 && rl.rlim_cur == 8ULL * 1024 * 1024);

	CHECK(fork() == -1 && errno == ENOSYS);
	CHECK(gai_strerror(EAI_FAIL) != NULL);

	/* strsignal names signals as glibc does (Postgres's crash log uses it) */
	CHECK(strcmp(strsignal(SIGKILL), "Killed") == 0);
	CHECK(strcmp(strsignal(SIGTERM), "Terminated") == 0);
	CHECK(strncmp(strsignal(200), "Unknown signal", 14) == 0);

	if (sigsetjmp(jb, 1) == 0)
		siglongjmp(jb, 7);
	else
		jumped = 1;
	CHECK(jumped == 1);

	/* System V shared memory backed by the kernel */
	{
		int			id = shmget(IPC_PRIVATE, 10000, IPC_CREAT | 0600);
		struct shmid_ds ds;
		char	   *p,
				   *heap;

		CHECK(id > 0);
		p = shmat(id, NULL, 0);
		CHECK(p != (void *) -1 && ((uintptr_t) p % 65536) == 0 && p[9999] == 0);
		/* A fixed attach cannot take an aligned address in the heap. */
		heap = aligned_alloc(16384, 16384);
		CHECK(heap != NULL);
		CHECK(shmat(id, heap, 0) == (void *) -1 && errno == EINVAL);
		free(heap);
		CHECK(shmctl(id, IPC_STAT, &ds) == 0 && ds.shm_nattch == 1 && ds.shm_segsz == 10000);
		CHECK(shmget(1234, 64, IPC_CREAT | IPC_EXCL | 0600) > 0);
		CHECK(shmget(1234, 64, IPC_CREAT | IPC_EXCL | 0600) == -1 && errno == EEXIST);
		CHECK(shmget(4321, 64, 0) == -1 && errno == ENOENT);
		CHECK(shmctl(id, IPC_RMID, NULL) == 0 && shmdt(p) == 0);
		CHECK(shmctl(id, IPC_STAT, &ds) == -1 && errno == EINVAL);
	}

	/* this program links no extension table, so dlopen fails cleanly */
	CHECK(dlopen("/pgmem/lib/postgresql/plpgsql.so", RTLD_NOW) == NULL);
	CHECK(dlerror() != NULL && dlerror() == NULL);

	/* dlsym on a NULL handle fails instead of reading low memory */
	CHECK(dlsym(NULL, "x") == NULL);
	CHECK(dlerror() != NULL);

	/* setitimer reports the value last set; a zero value disarms it */
	{
		struct itimerval v = {.it_value = {.tv_sec = 5}};
		struct itimerval z = {0};
		struct itimerval o;

		CHECK(setitimer(ITIMER_REAL, &v, NULL) == 0);
		CHECK(setitimer(ITIMER_REAL, NULL, &o) == 0 && o.it_value.tv_sec == 5);
		CHECK(setitimer(1, &v, NULL) == -1 && errno == EINVAL);
		CHECK(setitimer(ITIMER_REAL, &z, &o) == 0 && o.it_value.tv_sec == 5);
	}

	/* chmod and fchmod succeed on existing files (WASI has no mode bits) */
	{
		int			fd = open("/c.txt", O_CREAT | O_WRONLY, 0600);

		CHECK(fd >= 0 && fchmod(fd, 0600) == 0 && close(fd) == 0);
		CHECK(chmod("/c.txt", 0644) == 0);
		CHECK(chmod("/missing", 0644) == -1 && errno == ENOENT);
	}

	puts("compat ok");
	return 0;
}
