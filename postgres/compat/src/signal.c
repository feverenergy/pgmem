/*
 * POSIX signals under the pgmem kernel.
 *
 * Handlers live in this instance's memory. The pending, blocked and ignored
 * masks live in one block the kernel sees too (pgm.sig_register): other
 * processes, timers and Go set pending bits there from other threads, and
 * after every host call that returns with a deliverable signal pending the
 * kernel calls pgmem_signal_dispatch, which runs the handlers. So handlers
 * run when the program makes a host call, never in the middle of plain
 * computation, and a blocking call they interrupt fails with EINTR.
 *
 * There is no job control: SIGSTOP, which can be neither caught nor
 * blocked, terminates the process like any signal whose default action is
 * not to ignore it, as do SIGTSTP, SIGTTIN and SIGTTOU left at SIG_DFL;
 * SIGCONT is ignored by default.
 *
 * Bit signo-1 of a mask stands for signal signo. Only the kernel and this
 * file's dispatch touch pending, always atomically; blocked and ignored are
 * written only here, atomically, because the kernel reads them from other
 * threads.
 */
#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("sig_register") int32_t pgm_sig_register(void *state);
PGM_IMPORT("signal_exit") int32_t pgm_signal_exit(int32_t signo);

/*
 * The block the kernel shares. Its layout (struct pgmem_sigstate in
 * pgmem_compat.h) is part of the pgm ABI; it is global so that Postgres's
 * CHECK_FOR_INTERRUPTS can test it (PGMEM_SIGNALS_DELIVERABLE).
 */
_Alignas(8) struct pgmem_sigstate pgmem_sigstate;

static struct sigaction actions[_NSIG];

/*
 * wasi-libc defines SIG_IGN and SIG_ERR as the addresses of these two
 * functions. Their bodies differ so the linker can never fold them into one.
 */
void
__SIG_IGN(int signo)
{
	(void) signo;
}

void
__SIG_ERR(int signo)
{
	(void) signo;
	abort();
}

static int
valid_signal(int signo)
{
	return signo > 0 && signo < _NSIG;
}

static uint64_t
signal_bit(int signo)
{
	return (uint64_t) 1 << (signo - 1);
}

/* Signals that can be neither blocked nor caught. */
#define UNMASKABLE (signal_bit(SIGKILL) | signal_bit(SIGSTOP))

static int
ignored_by_default(int signo)
{
	return signo == SIGCHLD || signo == SIGURG || signo == SIGWINCH ||
		signo == SIGCONT;
}

static uint64_t
load(uint64_t *mask)
{
	return __atomic_load_n(mask, __ATOMIC_SEQ_CST);
}

static void
store(uint64_t *mask, uint64_t value)
{
	__atomic_store_n(mask, value, __ATOMIC_SEQ_CST);
}

/*
 * Registers the block with the kernel before main runs; signals sent
 * earlier were held by the kernel and become pending now.
 */
__attribute__((constructor))
static void
signal_init(void)
{
	uint64_t	ignored = 0;

	for (int signo = 1; signo < _NSIG; signo++)
		if (ignored_by_default(signo))
			ignored |= signal_bit(signo);
	store(&pgmem_sigstate.ignored, ignored);
	pgm_sig_register(&pgmem_sigstate);
}

/*
 * Runs every deliverable pending signal, lowest number first. While a
 * handler runs, its sa_mask and (without SA_NODEFER) the signal itself are
 * blocked. SIG_DFL on a signal that is not ignored by default ends the
 * process. The kernel calls this when a host call returns; raise,
 * sigprocmask and Postgres's CHECK_FOR_INTERRUPTS (patch 0004) call it
 * directly.
 */
__attribute__((export_name("pgmem_signal_dispatch")))
void
pgmem_signal_dispatch(void)
{
	for (;;)
	{
		uint64_t	ready = load(&pgmem_sigstate.pending) &
			~load(&pgmem_sigstate.blocked) & ~load(&pgmem_sigstate.ignored);
		int			signo;
		uint64_t	bit,
					saved,
					mask;
		const struct sigaction *sa;

		if (ready == 0)
			return;
		signo = __builtin_ctzll(ready) + 1;
		bit = signal_bit(signo);
		if ((__atomic_fetch_and(&pgmem_sigstate.pending, ~bit, __ATOMIC_SEQ_CST) & bit) == 0)
			continue;			/* a nested dispatch took it */
		sa = &actions[signo];
		if (sa->sa_handler == SIG_DFL)
		{
			pgm_signal_exit(signo);
			abort();			/* not reached */
		}
		if (sa->sa_handler == SIG_IGN)
			continue;
		saved = load(&pgmem_sigstate.blocked);
		mask = saved | sa->sa_mask;
		if (!(sa->sa_flags & SA_NODEFER))
			mask |= bit;
		store(&pgmem_sigstate.blocked, mask & ~UNMASKABLE);
		sa->sa_handler(signo);
		store(&pgmem_sigstate.blocked, saved);
	}
}

int
sigemptyset(sigset_t *set)
{
	*set = 0;
	return 0;
}

int
sigfillset(sigset_t *set)
{
	*set = ~(sigset_t) 0;
	return 0;
}

int
sigaddset(sigset_t *set, int signo)
{
	if (!valid_signal(signo))
	{
		errno = EINVAL;
		return -1;
	}
	*set |= signal_bit(signo);
	return 0;
}

int
sigdelset(sigset_t *set, int signo)
{
	if (!valid_signal(signo))
	{
		errno = EINVAL;
		return -1;
	}
	*set &= ~signal_bit(signo);
	return 0;
}

int
sigismember(const sigset_t *set, int signo)
{
	if (!valid_signal(signo))
	{
		errno = EINVAL;
		return -1;
	}
	return (*set & signal_bit(signo)) != 0;
}

/* Unblocking a pending signal delivers it before sigprocmask returns. */
int
sigprocmask(int how, const sigset_t *set, sigset_t *oldset)
{
	uint64_t	old = load(&pgmem_sigstate.blocked);
	uint64_t	new;

	if (set)
	{
		switch (how)
		{
			case SIG_BLOCK:
				new = old | *set;
				break;
			case SIG_UNBLOCK:
				new = old & ~*set;
				break;
			case SIG_SETMASK:
				new = *set;
				break;
			default:
				errno = EINVAL;
				return -1;
		}
		store(&pgmem_sigstate.blocked, new & ~UNMASKABLE);
	}
	if (oldset)
		*oldset = old;
	if (set && (old & ~new))
		pgmem_signal_dispatch();
	return 0;
}

int
pthread_sigmask(int how, const sigset_t *set, sigset_t *oldset)
{
	return sigprocmask(how, set, oldset) == 0 ? 0 : errno;
}

/*
 * Keeps the ignored mask exact: a signal is ignored when its handler is
 * SIG_IGN, or SIG_DFL and the signal is ignored by default. Ignoring a
 * signal discards it if it is pending, as POSIX requires.
 */
int
sigaction(int signo, const struct sigaction *act, struct sigaction *oldact)
{
	uint64_t	bit;

	if (!valid_signal(signo) ||
		(act && (signo == SIGKILL || signo == SIGSTOP)))
	{
		errno = EINVAL;
		return -1;
	}
	if (oldact)
		*oldact = actions[signo];
	if (!act)
		return 0;
	actions[signo] = *act;
	bit = signal_bit(signo);
	if (act->sa_handler == SIG_IGN ||
		(act->sa_handler == SIG_DFL && ignored_by_default(signo)))
	{
		__atomic_fetch_or(&pgmem_sigstate.ignored, bit, __ATOMIC_SEQ_CST);
		__atomic_fetch_and(&pgmem_sigstate.pending, ~bit, __ATOMIC_SEQ_CST);
	}
	else
		__atomic_fetch_and(&pgmem_sigstate.ignored, ~bit, __ATOMIC_SEQ_CST);
	return 0;
}

void		(*signal(int signo, void (*handler) (int))) (int)
{
	struct sigaction act;
	struct sigaction oldact;

	memset(&act, 0, sizeof(act));
	act.sa_handler = handler;
	if (sigaction(signo, &act, &oldact) < 0)
		return SIG_ERR;
	return oldact.sa_handler;
}

/* The signals that are pending because they are blocked. */
int
sigpending(sigset_t *set)
{
	*set = load(&pgmem_sigstate.pending) & load(&pgmem_sigstate.blocked);
	return 0;
}

/* Delivers signo to this process before returning, unless it is blocked. */
int
raise(int signo)
{
	uint64_t	bit;

	if (!valid_signal(signo))
	{
		errno = EINVAL;
		return -1;
	}
	bit = signal_bit(signo);
	if (load(&pgmem_sigstate.ignored) & bit)
		return 0;
	__atomic_fetch_or(&pgmem_sigstate.pending, bit, __ATOMIC_SEQ_CST);
	pgmem_signal_dispatch();
	return 0;
}
