/*
 * pgmem_compat.h - POSIX declarations that wasi-libc leaves out.
 *
 * Force-included into every Postgres translation unit (-include). The
 * implementations live in postgres/compat/src and are linked as
 * libpgmemcompat.a.
 */
#ifndef PGMEM_COMPAT_H
#define PGMEM_COMPAT_H

#include <sys/types.h>
#include <stdint.h>
#include <fcntl.h>
#include <setjmp.h>
#include <signal.h>
#include <stdio.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <time.h>

/* LLVM's setjmp lowering only recognizes direct setjmp calls. */
#undef sigsetjmp
#undef siglongjmp
#define sigsetjmp(env, savemask) setjmp(env)
#define siglongjmp longjmp

/* Signals: wasi-libc hides these behind __wasilibc_unmodified_upstream. */
#define SIG_BLOCK 0
#define SIG_UNBLOCK 1
#define SIG_SETMASK 2
#define SA_NOCLDSTOP 1
#define SA_RESTART 0x10000000
#ifndef SA_NODEFER
#define SA_NODEFER 0x40000000
#endif
struct sigaction
{
	void		(*sa_handler) (int);
	sigset_t	sa_mask;
	int			sa_flags;
};
int			sigemptyset(sigset_t *set);
int			sigfillset(sigset_t *set);
int			sigaddset(sigset_t *set, int signo);
int			sigdelset(sigset_t *set, int signo);
int			sigismember(const sigset_t *set, int signo);
int			sigprocmask(int how, const sigset_t *set, sigset_t *oldset);
int			pthread_sigmask(int how, const sigset_t *set, sigset_t *oldset);
int			sigaction(int signo, const struct sigaction *act, struct sigaction *oldact);
int			sigpending(sigset_t *set);
int			sigwait(const sigset_t *set, int *sig);
int			kill(pid_t pid, int signo);

/*
 * The signal block the kernel delivers into (signal.c registers it): bits
 * of pending, blocked and ignored signals. Postgres's miscadmin.h (patch
 * 0004) tests it at CHECK_FOR_INTERRUPTS, so a backend computing without
 * host calls still sees signals.
 */
struct pgmem_sigstate
{
	uint64_t	pending;
	uint64_t	blocked;
	uint64_t	ignored;
};
extern struct pgmem_sigstate pgmem_sigstate;
void		pgmem_signal_dispatch(void);

/*
 * PGMEM_CFI_POLL makes a host call (pgm.yield) every 4096th time it runs,
 * so a guest that only computes can still be killed, and Go can stop the
 * world, without wazero's preemption.
 */
extern uint32_t pgmem_cfi_ticks;
void		pgmem_cfi_yield(void);
#define PGMEM_CFI_POLL() \
	(__builtin_expect((++pgmem_cfi_ticks & 0xfff) == 0, 0) ? pgmem_cfi_yield() : (void) 0)
/*
 * Signals pending, not blocked and not ignored: what pgmem_signal_dispatch
 * would run. A signal that arrives while sigaction(SIG_IGN) runs can leave
 * a pending bit that is also ignored, and dispatch never clears it, so the
 * mask must leave it out or every CHECK_FOR_INTERRUPTS would dispatch.
 */
#define PGMEM_SIGNALS_DELIVERABLE() \
	((__atomic_load_n(&pgmem_sigstate.pending, __ATOMIC_RELAXED) & \
	  ~__atomic_load_n(&pgmem_sigstate.blocked, __ATOMIC_RELAXED) & \
	  ~__atomic_load_n(&pgmem_sigstate.ignored, __ATOMIC_RELAXED)) != 0)

struct itimerval
{
	struct timeval it_interval;
	struct timeval it_value;
};
#define ITIMER_REAL 0
int			setitimer(int which, const struct itimerval *value, struct itimerval *ovalue);
unsigned	alarm(unsigned seconds);
int			pause(void);

/* Processes and identity. */
pid_t		fork(void);
pid_t		getppid(void);
pid_t		setsid(void);
pid_t		getpgrp(void);
int			setpgid(pid_t pid, pid_t pgid);
uid_t		getuid(void);
uid_t		geteuid(void);
gid_t		getgid(void);
gid_t		getegid(void);
mode_t		umask(mode_t mask);
int			chown(const char *path, uid_t owner, gid_t group);
FILE	   *popen(const char *command, const char *mode);
int			pclose(FILE *stream);
void		tzset(void);

/*
 * Starts argv[0] (a program path the pgmem kernel knows) as a child process
 * in the current working directory. The child inherits every fd that is not
 * close-on-exec at the same number, except fd 3 (its own preopen); then
 * fdmap's nfdmap {child_fd, parent_fd} pairs make child_fd a duplicate of
 * parent_fd. Returns the child's pid, or -1 with errno set.
 */
pid_t		pgmem_spawn(char *const argv[], char *const envp[], const int *fdmap, int nfdmap);

/*
 * wasi-libc's chmod and fchmod fail with ENOSYS; WASI has no permission
 * bits. pgmem's versions succeed when the file exists.
 */
int			pgmem_chmod(const char *path, mode_t mode);
int			pgmem_fchmod(int fd, mode_t mode);
#define chmod(path, mode) pgmem_chmod(path, mode)
#define fchmod(fd, mode) pgmem_fchmod(fd, mode)

/*
 * wasi-libc's fcntl has no close-on-exec (F_GETFD always answers
 * FD_CLOEXEC) and no F_DUPFD. pgmem's keeps the flag in the kernel, which
 * pgmem_spawn consults: children inherit every other fd by number.
 */
int			pgmem_fcntl(int fd, int cmd,...);
#define fcntl(...) pgmem_fcntl(__VA_ARGS__)

/*
 * Sockets: declared by wasi-libc only outside wasip1. sock.c defines all
 * but sendto and recvfrom, which stay unsupported.
 */
int			socket(int domain, int type, int protocol);
int			bind(int fd, const struct sockaddr *addr, socklen_t len);
int			listen(int fd, int backlog);
int			connect(int fd, const struct sockaddr *addr, socklen_t len);
int			getsockname(int fd, struct sockaddr *__restrict addr, socklen_t *__restrict len);
int			getpeername(int fd, struct sockaddr *__restrict addr, socklen_t *__restrict len);
int			setsockopt(int fd, int level, int name, const void *value, socklen_t len);
ssize_t		sendto(int fd, const void *buf, size_t len, int flags, const struct sockaddr *addr, socklen_t alen);
ssize_t		recvfrom(int fd, void *buf, size_t len, int flags, struct sockaddr *addr, socklen_t *alen);
#ifndef SO_REUSEADDR
#define SO_REUSEADDR 2
#endif
#ifndef SO_ERROR
#define SO_ERROR 4
#endif
#ifndef SO_SNDBUF
#define SO_SNDBUF 7
#endif
#ifndef SO_RCVBUF
#define SO_RCVBUF 8
#endif
#ifndef SO_KEEPALIVE
#define SO_KEEPALIVE 9
#endif

/* Resource limits. */
typedef unsigned long long rlim_t;
struct rlimit
{
	rlim_t		rlim_cur;
	rlim_t		rlim_max;
};
#define RLIM_INFINITY (~0ULL)
#define RLIMIT_CORE 4
#define RLIMIT_STACK 3
#define RLIMIT_NOFILE 7
int			getrlimit(int resource, struct rlimit *rlim);
int			setrlimit(int resource, const struct rlimit *rlim);

#ifndef EHOSTDOWN
#define EHOSTDOWN 140
#endif

#endif							/* PGMEM_COMPAT_H */
