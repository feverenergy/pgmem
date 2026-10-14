/* Only so sysv_shmem.c and dsm_impl.c compile; pgmem uses its own shmem. */
#ifndef PGMEM_SYS_IPC_H
#define PGMEM_SYS_IPC_H
#include <sys/types.h>
typedef int key_t;
#define IPC_PRIVATE 0
#define IPC_CREAT 01000
#define IPC_EXCL 02000
#define IPC_RMID 0
#define IPC_STAT 2
struct ipc_perm
{
	key_t		__key;
	uid_t		uid;
	gid_t		gid;
	uid_t		cuid;
	gid_t		cgid;
	mode_t		mode;
};
#endif
