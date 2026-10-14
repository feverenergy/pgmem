#ifndef PGMEM_SYS_SHM_H
#define PGMEM_SYS_SHM_H
#include <stddef.h>
#include <sys/ipc.h>
struct shmid_ds
{
	struct ipc_perm shm_perm;
	size_t		shm_segsz;
	unsigned long shm_nattch;
};
int			shmget(key_t key, size_t size, int flags);
void	   *shmat(int id, const void *addr, int flags);
int			shmdt(const void *addr);
int			shmctl(int id, int cmd, struct shmid_ds *buf);
#endif
