/*
 * strsignal, which wasi-libc keeps in libwasi-emulated-signal (deliberately
 * not linked; see build/wasm-env.sh). With it, Postgres's configure finds
 * strsignal and the postmaster's crash log names the signal ("terminated by
 * signal 9: Killed"). The names are glibc's.
 */
#include <signal.h>
#include <stdio.h>
#include <string.h>

static const char *const names[] = {
	[SIGHUP] = "Hangup",
	[SIGINT] = "Interrupt",
	[SIGQUIT] = "Quit",
	[SIGILL] = "Illegal instruction",
	[SIGTRAP] = "Trace/breakpoint trap",
	[SIGABRT] = "Aborted",
	[SIGBUS] = "Bus error",
	[SIGFPE] = "Floating point exception",
	[SIGKILL] = "Killed",
	[SIGUSR1] = "User defined signal 1",
	[SIGSEGV] = "Segmentation fault",
	[SIGUSR2] = "User defined signal 2",
	[SIGPIPE] = "Broken pipe",
	[SIGALRM] = "Alarm clock",
	[SIGTERM] = "Terminated",
	[SIGSTKFLT] = "Stack fault",
	[SIGCHLD] = "Child exited",
	[SIGCONT] = "Continued",
	[SIGSTOP] = "Stopped (signal)",
	[SIGTSTP] = "Stopped",
	[SIGTTIN] = "Stopped (tty input)",
	[SIGTTOU] = "Stopped (tty output)",
	[SIGURG] = "Urgent I/O condition",
	[SIGXCPU] = "CPU time limit exceeded",
	[SIGXFSZ] = "File size limit exceeded",
	[SIGVTALRM] = "Virtual timer expired",
	[SIGPROF] = "Profiling timer expired",
	[SIGWINCH] = "Window changed",
	[SIGPOLL] = "I/O possible",
	[SIGPWR] = "Power failure",
	[SIGSYS] = "Bad system call",
};

char *
strsignal(int signo)
{
	static char unknown[32];

	if (signo > 0 && signo < (int) (sizeof(names) / sizeof(names[0])) && names[signo])
		return (char *) names[signo];
	snprintf(unknown, sizeof(unknown), "Unknown signal %d", signo);
	return unknown;
}
