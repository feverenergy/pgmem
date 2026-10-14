/*
 * Unnamed POSIX semaphores backed by the pgmem kernel (pgm.sem_*), which
 * keys each by where its sem_t is: in a shared segment (the same semaphore
 * for every process attached, at any address) or in the caller's private
 * memory. Postgres keeps its semaphores in the main shared segment
 * (posix_sema.c). wasi-libc's semaphores wait with memory.atomic.wait,
 * which never sees a post from another instance. Waits are interruptible:
 * a signal ends them with EINTR, and Postgres retries.
 *
 * Every unnamed-semaphore function that wasi-libc defines is defined here,
 * so none of its objects for them is linked. (Its named semaphores,
 * sem_open, sem_close and sem_unlink, are left alone: nothing uses them.)
 */
#include <errno.h>
#include <semaphore.h>
#include <stdint.h>
#include <time.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("sem_init") int32_t pgm_sem_init(uint32_t addr, uint32_t value);
PGM_IMPORT("sem_destroy") int32_t pgm_sem_destroy(uint32_t addr);
PGM_IMPORT("sem_post") int32_t pgm_sem_post(uint32_t addr);
PGM_IMPORT("sem_wait") int32_t pgm_sem_wait(uint32_t addr, int32_t mode, const void *ts);
PGM_IMPORT("sem_getvalue") int32_t pgm_sem_getvalue(uint32_t addr);

#define SEM_BLOCK 0
#define SEM_TRY 1
#define SEM_TIMED 2

static int
result(int32_t r)
{
	if (r < 0)
	{
		errno = -r;
		return -1;
	}
	return 0;
}

int
sem_init(sem_t *sem, int pshared, unsigned value)
{
	(void) pshared;				/* every semaphore can be shared */
	return result(pgm_sem_init((uint32_t) (uintptr_t) sem, value));
}

int
sem_destroy(sem_t *sem)
{
	return result(pgm_sem_destroy((uint32_t) (uintptr_t) sem));
}

int
sem_post(sem_t *sem)
{
	return result(pgm_sem_post((uint32_t) (uintptr_t) sem));
}

int
sem_wait(sem_t *sem)
{
	return result(pgm_sem_wait((uint32_t) (uintptr_t) sem, SEM_BLOCK, 0));
}

int
sem_trywait(sem_t *sem)
{
	return result(pgm_sem_wait((uint32_t) (uintptr_t) sem, SEM_TRY, 0));
}

int
sem_timedwait(sem_t *restrict sem, const struct timespec *restrict at)
{
	return result(pgm_sem_wait((uint32_t) (uintptr_t) sem, SEM_TIMED, at));
}

int
sem_getvalue(sem_t *restrict sem, int *restrict value)
{
	int32_t		r = pgm_sem_getvalue((uint32_t) (uintptr_t) sem);

	if (r < 0)
		return result(r);
	*value = r;
	return 0;
}
