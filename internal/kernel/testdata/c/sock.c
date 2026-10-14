/*
 * Unix-domain stream sockets under the pgmem kernel. Without arguments it
 * runs the checks and prints "sock ok"; child-* are children it starts,
 * and "echo PATH" is started from Go.
 */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define SELF "/fixtures/sock"

extern char **environ;

__attribute__((import_module("pgm"), import_name("sock_name")))
int32_t		pgm_sock_name(int32_t fd, int32_t which, char *buf, uint32_t buflen);

static volatile int usr1;

static void
on_usr1(int signo)
{
	(void) signo;
	usr1++;
}

static struct sockaddr_un
addr_of(const char *path)
{
	struct sockaddr_un a;

	memset(&a, 0, sizeof(a));
	a.sun_family = AF_UNIX;
	strcpy(a.sun_path, path);
	return a;
}

/* listen_at's sockets are close-on-exec, as Postgres's listen sockets are. */
static int
listen_at(const char *path, int backlog)
{
	struct sockaddr_un a = addr_of(path);
	int			fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);

	if (fd < 0 || bind(fd, (struct sockaddr *) &a, sizeof(a)) < 0 || listen(fd, backlog) < 0)
		return -1;
	return fd;
}

/* connect_to's sockets are close-on-exec: no child inherits them. */
static int
connect_to(const char *path)
{
	struct sockaddr_un a = addr_of(path);
	int			fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);

	if (fd < 0 || connect(fd, (struct sockaddr *) &a, sizeof(a)) < 0)
		return -1;
	return fd;
}

/* read_all reads until n bytes or EOF; returns the count. */
static int
read_all(int fd, char *buf, int n)
{
	int			got = 0;

	while (got < n)
	{
		int			r = (int) recv(fd, buf + got, n - got, 0);

		if (r <= 0)
			break;
		got += r;
	}
	return got;
}

/* spawn runs SELF mode arg [arg2] (arg2 may be NULL). */
static pid_t
spawn(char *mode, char *arg, char *arg2)
{
	char	   *argv[] = {SELF, mode, arg, arg2, NULL};

	return pgmem_spawn(argv, environ, NULL, 0);
}

static int
cloexec_pipe(int fds[2])
{
	if (pipe(fds) < 0)
		return -1;
	if (fcntl(fds[0], F_SETFD, FD_CLOEXEC) < 0 || fcntl(fds[1], F_SETFD, FD_CLOEXEC) < 0)
		return -1;
	return 0;
}

/*
 * spawn_ready runs SELF mode arg with stdout a pipe and returns once the
 * child has written its ready byte there (its handlers are installed).
 * With ready not NULL the pipe's read end is kept there for later bytes.
 */
static pid_t
spawn_ready(char *mode, char *arg, int *ready)
{
	char	   *argv[] = {SELF, mode, arg, NULL};
	int			p[2],
				map[2];
	char		r;
	pid_t		c;

	if (cloexec_pipe(p) < 0)
		return -1;
	map[0] = 1;					/* the child's stdout ... */
	map[1] = p[1];				/* ... is the ready pipe */
	c = pgmem_spawn(argv, environ, map, 1);
	close(p[1]);
	if (c > 0 && read(p[0], &r, 1) != 1)
		c = -1;
	if (ready != NULL && c > 0)
		*ready = p[0];
	else
		close(p[0]);
	return c;
}

/*
 * kill_until_exit sends SIGUSR1 to c until it exits, since a signal that
 * comes before the child blocks only runs its handler; true if c then
 * exited with 0.
 */
static int
kill_until_exit(pid_t c)
{
	int			st = 0,
				tries;

	for (tries = 0; tries < 1000; tries++)
	{
		if (waitpid(c, &st, WNOHANG) == c)
			break;
		kill(c, SIGUSR1);
		usleep(5000);
	}
	return tries < 1000 && WIFEXITED(st) && WEXITSTATUS(st) == 0;
}

static int
exited_ok(pid_t c)
{
	int			st;

	return c > 0 && waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0;
}

/* child-client PATH: ping, pong, half-close, then "late" and EOF. */
static int
child_client(const char *path)
{
	char		buf[16];
	int			fd = connect_to(path);

	CHECK(fd >= 0);
	CHECK(send(fd, "ping", 4, 0) == 4);
	CHECK(read_all(fd, buf, 4) == 4 && memcmp(buf, "pong", 4) == 0);
	CHECK(shutdown(fd, SHUT_WR) == 0);
	CHECK(read_all(fd, buf, 4) == 4 && memcmp(buf, "late", 4) == 0);
	CHECK(recv(fd, buf, sizeof(buf), 0) == 0);
	close(fd);
	return 0;
}

/* child-hello PATH: says hello, expects bye then EOF. */
static int
child_hello(const char *path)
{
	char		buf[8];
	int			fd = connect_to(path);

	CHECK(fd >= 0);
	CHECK(send(fd, "hello", 5, 0) == 5);
	CHECK(read_all(fd, buf, 3) == 3 && memcmp(buf, "bye", 3) == 0);
	CHECK(recv(fd, buf, sizeof(buf), 0) == 0);
	return 0;
}

/* child-serve FD: talks to the client on an fd inherited by number. */
static int
child_serve(int fd)
{
	char		buf[8];

	CHECK(read_all(fd, buf, 5) == 5 && memcmp(buf, "hello", 5) == 0);
	CHECK(send(fd, "bye", 3, 0) == 3);
	close(fd);
	return 0;
}

/*
 * child-blocked HOW: a signal interrupts a blocking accept, recv or poll
 * (HOW) on a socket of its own: the call fails with EINTR once the handler
 * ran.
 */
static int
child_blocked(const char *how)
{
	struct sigaction sa = {0};
	struct pollfd pfd;
	char		path[32],
				b;
	int			l,
				c,
				s;

	sa.sa_handler = on_usr1;
	CHECK(sigaction(SIGUSR1, &sa, NULL) == 0);
	snprintf(path, sizeof(path), "/tmp/b-%s.sock", how);
	l = listen_at(path, 1);
	CHECK(l >= 0);
	if (strcmp(how, "accept") == 0)
	{
		CHECK(write(1, "r", 1) == 1);	/* ready: the parent's pipe */
		errno = 0;
		CHECK(accept(l, NULL, NULL) < 0 && errno == EINTR && usr1 >= 1);
		return 0;
	}
	c = connect_to(path);
	CHECK(c >= 0);
	s = accept(l, NULL, NULL);
	CHECK(s >= 0);
	CHECK(write(1, "r", 1) == 1);
	errno = 0;
	if (strcmp(how, "recv") == 0)
		CHECK(recv(s, &b, 1, 0) < 0 && errno == EINTR && usr1 >= 1);
	else
	{
		pfd.fd = s;
		pfd.events = POLLIN;
		CHECK(poll(&pfd, 1, -1) < 0 && errno == EINTR && usr1 >= 1);
	}
	return 0;
}

/* child-recv FD: blocks in recv on an inherited fd until killed. */
static int
child_recv(int fd)
{
	char		b;

	CHECK(write(1, "r", 1) == 1);
	recv(fd, &b, 1, 0);
	return 1;					/* the parent sends nothing: never here */
}

/*
 * child-connect PATH: a blocking connect to a listener whose queue is
 * full waits (a signal interrupts it), then goes through once the parent
 * accepts.
 */
static int
child_connect(const char *path)
{
	struct sigaction sa = {0};
	struct sockaddr_un a = addr_of(path);
	int			fd = socket(AF_UNIX, SOCK_STREAM, 0);

	CHECK(fd >= 0);
	sa.sa_handler = on_usr1;
	CHECK(sigaction(SIGUSR1, &sa, NULL) == 0);
	CHECK(write(1, "r", 1) == 1);
	errno = 0;
	CHECK(connect(fd, (struct sockaddr *) &a, sizeof(a)) < 0 && errno == EINTR && usr1 >= 1);
	sa.sa_handler = SIG_IGN;	/* no more interruptions */
	CHECK(sigaction(SIGUSR1, &sa, NULL) == 0);
	CHECK(write(1, "i", 1) == 1);
	CHECK(connect(fd, (struct sockaddr *) &a, sizeof(a)) == 0);
	CHECK(send(fd, "x", 1, 0) == 1);
	close(fd);
	return 0;
}

/*
 * child-echo-conn FD LISTENER: echoes until EOF on an inherited fd. The
 * listener's fd, close-on-exec in the parent, must not be open here.
 */
static int
child_echo_conn(int fd, int listener)
{
	char		buf[4096];

	CHECK(fcntl(listener, F_GETFD) < 0 && errno == EBADF);

	for (;;)
	{
		ssize_t		n = recv(fd, buf, sizeof(buf), 0);

		if (n == 0)
			return 0;
		CHECK(n > 0);
		for (ssize_t done = 0; done < n;)
		{
			ssize_t		w = send(fd, buf + done, n - done, 0);

			CHECK(w > 0);
			done += w;
		}
	}
}

/*
 * echo PATH: listens, writes "r" to stdout, and hands every connection to
 * a child that echoes, as the postmaster hands a client to a backend. The
 * listener is close-on-exec, as Postgres's are, so no child inherits it.
 */
static int
echo(const char *path)
{
	char		larg[16];
	int			l = listen_at(path, 64);

	CHECK(l >= 0);
	snprintf(larg, sizeof(larg), "%d", l);
	CHECK(write(1, "r", 1) == 1);
	for (;;)
	{
		char		arg[16];
		int			fd = accept(l, NULL, NULL);
		pid_t		c;

		CHECK(fd >= 0);
		snprintf(arg, sizeof(arg), "%d", fd);
		c = spawn("child-echo-conn", arg, larg);
		CHECK(c > 0);
		close(fd);
		while (waitpid(-1, NULL, WNOHANG) > 0)
			;
	}
}

static int
errors(void)
{
	struct sockaddr_un a = addr_of("/tmp/missing.sock");
	struct sockaddr_un longp;
	struct stat st;
	int			fd = socket(AF_UNIX, SOCK_STREAM, 0),
				l;

	CHECK(socket(AF_INET, SOCK_STREAM, 0) < 0 && errno == EAFNOSUPPORT);
	CHECK(fd >= 0);
	CHECK(connect(fd, (struct sockaddr *) &a, sizeof(a)) < 0 && errno == ENOENT);
	CHECK(close(open("/tmp/plain", O_CREAT | O_WRONLY, 0600)) == 0);
	a = addr_of("/tmp/plain");
	CHECK(connect(fd, (struct sockaddr *) &a, sizeof(a)) < 0 && errno == ECONNREFUSED);
	l = listen_at("/tmp/e.sock", 1);
	CHECK(l >= 0);
	CHECK(stat("/tmp/e.sock", &st) == 0);
	CHECK(listen_at("/tmp/e.sock", 1) < 0 && errno == EADDRINUSE);
	memset(&longp, 0, sizeof(longp));
	longp.sun_family = AF_UNIX;
	memset(longp.sun_path, 'x', sizeof(longp.sun_path));
	CHECK(bind(fd, (struct sockaddr *) &longp, sizeof(longp)) < 0 && errno == EINVAL);
	/* Unlinked path: the listener is unreachable. */
	CHECK(unlink("/tmp/e.sock") == 0);
	a = addr_of("/tmp/e.sock");
	CHECK(connect(fd, (struct sockaddr *) &a, sizeof(a)) < 0 && errno == ENOENT);
	/* Rebinding a bound socket fails and leaves no file at the new path. */
	a = addr_of("/tmp/b1.sock");
	CHECK(bind(fd, (struct sockaddr *) &a, sizeof(a)) == 0);
	a = addr_of("/tmp/b2.sock");
	CHECK(bind(fd, (struct sockaddr *) &a, sizeof(a)) < 0 && errno == EINVAL);
	CHECK(stat("/tmp/b2.sock", &st) < 0 && errno == ENOENT);
	{
		int			type = 0;
		socklen_t	len = sizeof(type);

		CHECK(getsockopt(l, SOL_SOCKET, SO_TYPE, &type, &len) == 0 && type == SOCK_STREAM);
		CHECK(getsockopt(1, SOL_SOCKET, SO_TYPE, &type, &len) < 0 && errno == ENOTSOCK);
		CHECK(setsockopt(l, SOL_SOCKET, SO_REUSEADDR, &type, sizeof(type)) == 0);
	}
	close(l);
	close(fd);
	return 0;
}

/* exchange: poll readiness, accept4 flags, names, half-close. */
static int
exchange(void)
{
	struct sockaddr_un peer;
	struct sockaddr_un self;
	socklen_t	plen = sizeof(peer),
				slen = sizeof(self);
	struct pollfd pfd;
	char		buf[16];
	int			l = listen_at("/tmp/x.sock", 4),
				fd;
	pid_t		c;

	CHECK(l >= 0);
	CHECK(getsockname(l, (struct sockaddr *) &self, &slen) == 0 && strcmp(self.sun_path, "/tmp/x.sock") == 0);
	/* A short buffer gets the start of the name and the full length. */
	memset(&self, '#', sizeof(self));
	slen = offsetof(struct sockaddr_un, sun_path) + 4;
	CHECK(getsockname(l, (struct sockaddr *) &self, &slen) == 0);
	CHECK(slen == offsetof(struct sockaddr_un, sun_path) + sizeof("/tmp/x.sock"));
	CHECK(memcmp(self.sun_path, "/tmp#", 5) == 0);
	memset(buf, '#', sizeof(buf));
	CHECK(pgm_sock_name(l, 0, buf, 4) == (int32_t) strlen("/tmp/x.sock") && memcmp(buf, "/tmp#", 5) == 0);
	slen = sizeof(self);
	CHECK(fcntl(l, F_SETFL, O_NONBLOCK) == 0);
	CHECK(accept(l, NULL, NULL) < 0 && errno == EAGAIN);
	CHECK(fcntl(l, F_SETFL, 0) == 0);
	c = spawn("child-client", "/tmp/x.sock", NULL);
	CHECK(c > 0);
	pfd.fd = l;
	pfd.events = POLLIN;
	CHECK(poll(&pfd, 1, 5000) == 1 && (pfd.revents & POLLIN));
	fd = accept4(l, (struct sockaddr *) &peer, &plen, SOCK_CLOEXEC | SOCK_NONBLOCK);
	CHECK(fd >= 0);
	CHECK(plen == offsetof(struct sockaddr_un, sun_path) && peer.sun_family == AF_UNIX);
	CHECK((fcntl(fd, F_GETFD) & FD_CLOEXEC) && (fcntl(fd, F_GETFL) & O_NONBLOCK));
	CHECK(fcntl(fd, F_SETFL, 0) == 0);
	slen = sizeof(self);
	CHECK(getsockname(fd, (struct sockaddr *) &self, &slen) == 0 && strcmp(self.sun_path, "/tmp/x.sock") == 0);
	pfd.fd = fd;
	CHECK(poll(&pfd, 1, 5000) == 1 && (pfd.revents & POLLIN));
	CHECK(read_all(fd, buf, 4) == 4 && memcmp(buf, "ping", 4) == 0);
	CHECK(send(fd, "pong", 4, 0) == 4);
	CHECK(recv(fd, buf, sizeof(buf), 0) == 0);	/* client's SHUT_WR */
	CHECK(send(fd, "late", 4, 0) == 4);			/* the other way still works */
	close(fd);
	CHECK(exited_ok(c));
	close(l);
	return 0;
}

/* epipe: a peer that closed gives EPIPE on write and EOF on read. */
static int
epipe(void)
{
	int			l = listen_at("/tmp/p.sock", 1),
				c,
				s;
	char		b;

	CHECK(l >= 0);
	c = connect_to("/tmp/p.sock");
	CHECK(c >= 0);
	s = accept(l, NULL, NULL);
	CHECK(s >= 0);
	close(s);
	CHECK(send(c, "x", 1, 0) < 0 && errno == EPIPE);
	CHECK(recv(c, &b, 1, 0) == 0);
	close(c);
	close(l);
	return 0;
}

/* handoff: the EXEC_BACKEND shape: a child serves an inherited client fd. */
static int
handoff(void)
{
	char		arg[16];
	int			l = listen_at("/tmp/h.sock", 1),
				fd;
	pid_t		client,
				server;

	CHECK(l >= 0);
	client = spawn("child-hello", "/tmp/h.sock", NULL);
	CHECK(client > 0);
	fd = accept(l, NULL, NULL);
	CHECK(fd >= 0);
	snprintf(arg, sizeof(arg), "%d", fd);
	server = spawn("child-serve", arg, NULL);
	CHECK(server > 0);
	close(fd);
	CHECK(exited_ok(server) && exited_ok(client));
	close(l);
	return 0;
}

/* interrupt: SIGUSR1 interrupts a child's blocking accept, recv and poll. */
static int
interrupt(void)
{
	char	   *hows[] = {"accept", "recv", "poll"};

	for (int i = 0; i < 3; i++)
	{
		pid_t		c = spawn_ready("child-blocked", hows[i], NULL);

		CHECK(c > 0);
		if (!kill_until_exit(c))
		{
			printf("FAIL interrupt %s\n", hows[i]);
			return 1;
		}
	}
	return 0;
}

/*
 * killed: a peer killed while blocked in recv hangs up: poll sees POLLIN
 * and POLLHUP, recv reads EOF and send fails with EPIPE.
 */
static int
killed(void)
{
	struct pollfd pfd;
	char		arg[16],
				b;
	int			l = listen_at("/tmp/k.sock", 1),
				c,
				s,
				st;
	pid_t		child;

	CHECK(l >= 0);
	c = connect_to("/tmp/k.sock");
	CHECK(c >= 0);
	s = accept(l, NULL, NULL);
	CHECK(s >= 0);
	snprintf(arg, sizeof(arg), "%d", s);
	child = spawn_ready("child-recv", arg, NULL);
	CHECK(child > 0);
	close(s);					/* the child holds the only server end */
	CHECK(kill(child, SIGKILL) == 0);
	CHECK(waitpid(child, &st, 0) == child && WIFSIGNALED(st) && WTERMSIG(st) == SIGKILL);
	pfd.fd = c;
	pfd.events = POLLIN;
	CHECK(poll(&pfd, 1, 5000) == 1 && (pfd.revents & POLLIN) && (pfd.revents & POLLHUP));
	CHECK(recv(c, &b, 1, 0) == 0);
	CHECK(send(c, "x", 1, 0) < 0 && errno == EPIPE);
	close(c);
	close(l);
	return 0;
}

/*
 * backlog: a blocking connect to a listener whose queue is full waits (a
 * signal interrupting it proves it blocked) and goes through once the
 * queued connection is accepted.
 */
static int
backlog(void)
{
	struct sockaddr_un a = addr_of("/tmp/q.sock");
	struct pollfd pfd;
	char		b;
	int			l = listen_at("/tmp/q.sock", 1),
				first,
				nb,
				ready = -1,
				s,
				tries;
	pid_t		c;

	CHECK(l >= 0);
	first = connect_to("/tmp/q.sock");
	CHECK(first >= 0);
	nb = socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
	CHECK(nb >= 0);
	CHECK(connect(nb, (struct sockaddr *) &a, sizeof(a)) < 0 && errno == EAGAIN);	/* full */
	close(nb);
	c = spawn_ready("child-connect", "/tmp/q.sock", &ready);
	CHECK(c > 0);
	/* SIGUSR1 until the child reports that its connect was interrupted. */
	pfd.fd = ready;
	pfd.events = POLLIN;
	for (tries = 0; tries < 1000; tries++)
	{
		CHECK(kill(c, SIGUSR1) == 0);
		if (poll(&pfd, 1, 5) == 1)
			break;
	}
	CHECK(tries < 1000 && read(ready, &b, 1) == 1 && b == 'i');
	close(ready);
	s = accept(l, NULL, NULL);	/* first's: makes room for the child */
	CHECK(s >= 0);
	close(s);
	s = accept(l, NULL, NULL);
	CHECK(s >= 0 && read_all(s, &b, 1) == 1 && b == 'x');
	close(s);
	close(first);
	CHECK(exited_ok(c));
	close(l);
	return 0;
}

int
main(int argc, char **argv)
{
	if (argc == 3 && strcmp(argv[1], "child-client") == 0)
		return child_client(argv[2]);
	if (argc == 3 && strcmp(argv[1], "child-hello") == 0)
		return child_hello(argv[2]);
	if (argc == 3 && strcmp(argv[1], "child-serve") == 0)
		return child_serve(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-blocked") == 0)
		return child_blocked(argv[2]);
	if (argc == 3 && strcmp(argv[1], "child-recv") == 0)
		return child_recv(atoi(argv[2]));
	if (argc == 3 && strcmp(argv[1], "child-connect") == 0)
		return child_connect(argv[2]);
	if (argc == 4 && strcmp(argv[1], "child-echo-conn") == 0)
		return child_echo_conn(atoi(argv[2]), atoi(argv[3]));
	if (argc == 3 && strcmp(argv[1], "echo") == 0)
		return echo(argv[2]);
	if (errors() || exchange() || epipe() || handoff() || interrupt() || killed() || backlog())
		return 1;
	printf("sock ok\n");
	return 0;
}
