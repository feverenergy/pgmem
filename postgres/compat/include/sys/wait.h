#ifndef PGMEM_SYS_WAIT_H
#define PGMEM_SYS_WAIT_H
#include <sys/types.h>
#define WNOHANG 1
#define WTERMSIG(s) ((s) & 0x7f)
#define WEXITSTATUS(s) (((s) >> 8) & 0xff)
#define WIFEXITED(s) (WTERMSIG(s) == 0)
#define WIFSIGNALED(s) (WTERMSIG(s) != 0 && WTERMSIG(s) != 0x7f)
#define WIFSTOPPED(s) 0
pid_t		waitpid(pid_t pid, int *status, int options);
pid_t		wait(int *status);
#endif
