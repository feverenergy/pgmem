/*
 * System V shared memory backed by the pgmem kernel (pgm.shm_*): the host
 * keeps one shared object per segment and maps it into the linear memory
 * of every process that attaches it, so they all see the same bytes.
 *
 * shmat(id, addr) maps at addr, a multiple of 16 KiB. The range must be
 * one this process reserved before, or lie wholly at or above the current
 * end of memory: the memory then grows past it and the range is reserved
 * for fixed attaches, while the heap continues above it. Postgres's main
 * segment attaches this way under EXEC_BACKEND (PG_SHMEM_ADDR), at the
 * same address in every process. shmat(id, NULL) maps over a 64 KiB-aligned
 * heap block instead (dynamic shared memory; addresses differ per process).
 *
 * Fresh segments are zero (the host's object), so nothing is cleared here:
 * touching every page would make it resident for nothing.
 */
#include <errno.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/shm.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("shm_get") int32_t pgm_shm_get(int32_t key, uint32_t size, int32_t flags);
PGM_IMPORT("shm_attach") int32_t pgm_shm_attach(int32_t id, uint32_t addr);
PGM_IMPORT("shm_detach") int32_t pgm_shm_detach(uint32_t addr);
PGM_IMPORT("shm_stat") int32_t pgm_shm_stat(int32_t id, void *buf);
PGM_IMPORT("shm_rmid") int32_t pgm_shm_rmid(int32_t id);

/* What pgm.shm_stat writes. */
struct pgmem_shm_stat
{
	int32_t		key;
	uint32_t	size;			/* as requested */
	uint32_t	mapped;			/* bytes an attach maps: size rounded up */
	uint32_t	nattch;
	int32_t		cpid;
	int32_t		lpid;
};

#define WASM_PAGE 65536u
#define SHM_ALIGN 16384u
#define MAX_ATTACH 64
#define MAX_RESERVED 8

struct attachment
{
	uintptr_t	addr;			/* 0: free slot */
	int			heap;			/* a heap block, freed at detach */
};

struct range
{
	uint64_t	lo,
				hi;
};

static struct attachment attached[MAX_ATTACH];
static struct range reserved[MAX_RESERVED];
static int	nreserved;

static int
fail(int32_t r)
{
	errno = -r;
	return -1;
}

int
shmget(key_t key, size_t size, int flags)
{
	int32_t		r = pgm_shm_get(key, (uint32_t) size, flags);

	return r < 0 ? fail(r) : r;
}

/*
 * reserve makes [lo, hi) available for fixed attaches: it is inside a
 * range reserved before, or wholly at or above the end of memory, which
 * then grows to cover it. Anything below the end of memory may be heap.
 */
static int
reserve(uint64_t lo, uint64_t hi)
{
	uint64_t	end = (uint64_t) __builtin_wasm_memory_size(0) * WASM_PAGE;

	for (int i = 0; i < nreserved; i++)
		if (lo >= reserved[i].lo && hi <= reserved[i].hi)
			return 0;
	if (lo < end || hi > ((uint64_t) 1 << 32))
	{
		errno = EINVAL;
		return -1;
	}
	if (nreserved == MAX_RESERVED)
	{
		errno = ENOMEM;
		return -1;
	}
	if (__builtin_wasm_memory_grow(0, (size_t) ((hi - end + WASM_PAGE - 1) / WASM_PAGE)) == (size_t) -1)
	{
		errno = ENOMEM;
		return -1;
	}
	reserved[nreserved].lo = lo;
	reserved[nreserved].hi = hi;
	nreserved++;
	return 0;
}

void *
shmat(int id, const void *addr, int flags)
{
	struct pgmem_shm_stat st;
	struct attachment *slot = NULL;
	uintptr_t	at;
	int32_t		r;

	(void) flags;				/* SHM_RDONLY and SHM_RND are not supported */
	for (int i = 0; i < MAX_ATTACH && slot == NULL; i++)
		if (attached[i].addr == 0)
			slot = &attached[i];
	if (slot == NULL)
	{
		errno = EMFILE;
		return (void *) -1;
	}
	if ((r = pgm_shm_stat(id, &st)) < 0)
		return (void *) (intptr_t) fail(r);
	if (addr != NULL)
	{
		at = (uintptr_t) addr;
		if (at % SHM_ALIGN != 0)
		{
			errno = EINVAL;
			return (void *) -1;
		}
		if (reserve(at, (uint64_t) at + st.mapped) < 0)
			return (void *) -1;
	}
	else
	{
		void	   *block = aligned_alloc(WASM_PAGE, (st.mapped + WASM_PAGE - 1) & ~(WASM_PAGE - 1));

		if (block == NULL)
		{
			errno = ENOMEM;
			return (void *) -1;
		}
		at = (uintptr_t) block;
	}
	if ((r = pgm_shm_attach(id, (uint32_t) at)) < 0)
	{
		/*
		 * A fixed attach keeps its reservation: harmless, since a retry at
		 * the same address finds the range reserved and works.
		 */
		if (addr == NULL)
			free((void *) at);
		return (void *) (intptr_t) fail(r);
	}
	slot->addr = at;
	slot->heap = addr == NULL;
	return (void *) at;
}

int
shmdt(const void *addr)
{
	for (int i = 0; i < MAX_ATTACH; i++)
	{
		struct attachment *a = &attached[i];
		int32_t		r;

		if (a->addr == 0 || a->addr != (uintptr_t) addr)
			continue;
		if ((r = pgm_shm_detach((uint32_t) a->addr)) < 0)
			return fail(r);
		/* The host put private pages back, so free may write there. */
		if (a->heap)
			free((void *) a->addr);
		a->addr = 0;
		return 0;
	}
	errno = EINVAL;
	return -1;
}

int
shmctl(int id, int cmd, struct shmid_ds *buf)
{
	struct pgmem_shm_stat st;
	int32_t		r;

	switch (cmd)
	{
		case IPC_STAT:
			if ((r = pgm_shm_stat(id, &st)) < 0)
				return fail(r);
			memset(buf, 0, sizeof(*buf));
			buf->shm_perm.__key = st.key;
			buf->shm_perm.uid = buf->shm_perm.cuid = geteuid();
			buf->shm_perm.gid = buf->shm_perm.cgid = getegid();
			buf->shm_perm.mode = 0600;
			buf->shm_segsz = st.size;
			buf->shm_nattch = st.nattch;
			return 0;
		case IPC_RMID:
			r = pgm_shm_rmid(id);
			return r < 0 ? fail(r) : 0;
		default:
			errno = EINVAL;
			return -1;
	}
}
