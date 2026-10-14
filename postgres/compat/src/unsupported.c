/*
 * Calls the pgmem kernel does not provide: sigwait, sendto and recvfrom, and
 * POSIX shared memory. They fail with ENOSYS. fork never will work. Children
 * start with pgmem_spawn instead (process.c).
 */
#include <errno.h>
#include <stddef.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/shm.h>

#define UNSUPPORTED(ret) do { errno = ENOSYS; return (ret); } while (0)

pid_t fork(void) { UNSUPPORTED(-1); }
int sigwait(const sigset_t *set, int *sig) { (void) set; (void) sig; return ENOSYS; }
ssize_t sendto(int fd, const void *buf, size_t len, int flags, const struct sockaddr *addr, socklen_t alen) { (void) fd; (void) buf; (void) len; (void) flags; (void) addr; (void) alen; UNSUPPORTED(-1); }
ssize_t recvfrom(int fd, void *buf, size_t len, int flags, struct sockaddr *addr, socklen_t *alen) { (void) fd; (void) buf; (void) len; (void) flags; (void) addr; (void) alen; UNSUPPORTED(-1); }
int shm_open(const char *name, int flags, mode_t mode) { (void) name; (void) flags; (void) mode; UNSUPPORTED(-1); }
int shm_unlink(const char *name) { (void) name; UNSUPPORTED(-1); }
