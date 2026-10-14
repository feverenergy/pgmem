/*
 * popen, pclose and system without a shell. A small parser handles exactly
 * what initdb and src/common/exec.c build: words; '...' and "..." quoting;
 * backslash escapes; and redirection of fds 0-2 to /dev/null with <, >,
 * 1>, 2> or n>&m. Anything else (pipes, variables, globs, other files) fails
 * with EINVAL or ENOTSUP rather than running something different from what
 * was asked.
 */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

/* Characters that mean shell features we do not implement. */
#define UNSUPPORTED_CHARS "|&;()$`*?[]{}~#\n"

struct buf
{
	char	   *p;
	size_t		len;
	size_t		cap;
};

struct words
{
	char	  **v;
	int			n;
	int			cap;
};

static int
buf_putc(struct buf *b, char c)
{
	if (b->len + 1 >= b->cap)
	{
		size_t		ncap = b->cap ? b->cap * 2 : 64;
		char	   *n = realloc(b->p, ncap);

		if (n == NULL)
			return -1;
		b->p = n;
		b->cap = ncap;
	}
	b->p[b->len++] = c;
	b->p[b->len] = '\0';
	return 0;
}

static int
words_add(struct words *w, const struct buf *b)
{
	char	   *copy;

	if (w->n + 2 > w->cap)
	{
		int			ncap = w->cap ? w->cap * 2 : 8;
		char	  **n = realloc(w->v, ncap * sizeof(char *));

		if (n == NULL)
			return -1;
		w->v = n;
		w->cap = ncap;
	}
	copy = strdup(b->p ? b->p : "");
	if (copy == NULL)
		return -1;
	w->v[w->n++] = copy;
	w->v[w->n] = NULL;
	return 0;
}

static void
words_free(struct words *w)
{
	for (int i = 0; i < w->n; i++)
		free(w->v[i]);
	free(w->v);
	memset(w, 0, sizeof(*w));
}

/*
 * read_word reads one possibly quoted word at *pp into b. A word ends at a
 * space or tab, or at an unquoted < or >. Unquoted newlines and #, and
 * backslash-newline, are rejected: sh would treat them as command
 * separators, comments or line continuations. Returns 0, or -1 with errno set.
 */
static int
read_word(const char **pp, struct buf *b)
{
	const char *p = *pp;
	int			any = 0;

	b->len = 0;
	if (b->p)
		b->p[0] = '\0';
	while (*p && *p != ' ' && *p != '\t' && *p != '<' && *p != '>')
	{
		if (*p == '\'')
		{
			for (p++; *p && *p != '\''; p++)
				if (buf_putc(b, *p) < 0)
					return -1;
			if (*p != '\'')
				goto syntax;
			p++;
		}
		else if (*p == '"')
		{
			for (p++; *p && *p != '"'; p++)
			{
				if (*p == '\\' && p[1] == '\n')
					goto syntax;
				else if (*p == '\\' && strchr("\"\\$`", p[1]) && p[1])
					p++;
				else if (*p == '$' || *p == '`')
					goto syntax;
				if (buf_putc(b, *p) < 0)
					return -1;
			}
			if (*p != '"')
				goto syntax;
			p++;
		}
		else if (*p == '\\' && p[1] && p[1] != '\n')
		{
			if (buf_putc(b, p[1]) < 0)
				return -1;
			p += 2;
		}
		else if (strchr(UNSUPPORTED_CHARS, *p))
			goto syntax;
		else if (buf_putc(b, *p++) < 0)
			return -1;
		any = 1;
	}
	if (!any)
		goto syntax;
	*pp = p;
	return 0;

syntax:
	errno = EINVAL;
	return -1;
}

/*
 * parse_command splits cmd into argv and applies redirections to stdio[],
 * the parent fd that each child fd 0-2 becomes. The first /dev/null
 * redirection opens /dev/null into *nullfd. Returns 0, or -1 with errno set.
 */
static int
parse_command(const char *cmd, struct words *argv, int stdio[3], int *nullfd)
{
	const char *p = cmd;
	struct buf	word = {0};
	int			r = -1;

	for (;;)
	{
		int			redir = -1;

		while (*p == ' ' || *p == '\t')
			p++;
		if (*p == '\0')
			break;
		if ((p[0] == '1' || p[0] == '2') && p[1] == '>')
		{
			redir = p[0] - '0';
			p += 2;
		}
		else if (*p == '>' || *p == '<')
			redir = *p++ == '>' ? 1 : 0;
		if (redir >= 0 && *p == '&')
		{
			if (p[1] < '0' || p[1] > '2')
			{
				errno = EINVAL;
				goto out;
			}
			if (p[2] != '\0' && p[2] != ' ' && p[2] != '\t')
			{
				errno = EINVAL;
				goto out;
			}
			stdio[redir] = stdio[p[1] - '0'];
			p += 2;
			continue;
		}
		while (redir >= 0 && (*p == ' ' || *p == '\t'))
			p++;
		if (read_word(&p, &word) < 0)
			goto out;
		if (redir < 0)
		{
			/* a number glued to < or > other than 1> and 2> is an fd we do not support */
			if ((*p == '<' || *p == '>') && strspn(word.p, "0123456789") == word.len)
			{
				errno = EINVAL;
				goto out;
			}
			if (words_add(argv, &word) < 0)
				goto out;
			continue;
		}
		if (strcmp(word.p, "/dev/null") != 0)
		{
			errno = ENOTSUP;
			goto out;
		}
		if (*nullfd < 0)
		{
			/* close-on-exec: the child gets it only where the map says */
			if ((*nullfd = open("/dev/null", O_RDWR)) < 0)
				goto out;
			if (fcntl(*nullfd, F_SETFD, FD_CLOEXEC) < 0)
				goto out;
		}
		stdio[redir] = *nullfd;
	}
	if (argv->n == 0)
		errno = EINVAL;
	else
		r = 0;
out:
	free(word.p);
	return r;
}

/*
 * spawn_command starts cmd. If child_fd is 0 or 1, that fd of the child is
 * parent_fd (a pipe end); otherwise the child's fds 0-2 are ours.
 */
static pid_t
spawn_command(const char *cmd, int child_fd, int parent_fd)
{
	struct words argv = {0};
	int			stdio[3] = {0, 1, 2};
	int			nullfd = -1;
	int			map[6];
	int			nmap = 0;
	pid_t		pid = -1;
	int			save;

	if (child_fd >= 0)
		stdio[child_fd] = parent_fd;
	if (parse_command(cmd, &argv, stdio, &nullfd) == 0)
	{
		for (int i = 0; i < 3; i++)
			if (stdio[i] != i)
			{
				map[2 * nmap] = i;
				map[2 * nmap + 1] = stdio[i];
				nmap++;
			}
		pid = pgmem_spawn(argv.v, environ, map, nmap);
	}
	save = errno;
	words_free(&argv);
	if (nullfd >= 0)
		close(nullfd);
	errno = save;
	return pid;
}

static int
wait_for(pid_t pid)
{
	int			status;

	while (waitpid(pid, &status, 0) < 0)
		if (errno != EINTR)
			return -1;
	return status;
}

/* popen'd streams and their children. */
#define MAX_POPEN 16
static struct
{
	FILE	   *f;
	pid_t		pid;
}			popened[MAX_POPEN];

FILE *
popen(const char *command, const char *mode)
{
	int			writing;
	int			fds[2];
	int			keep,
				give,
				slot,
				save;
	pid_t		pid;
	FILE	   *f;

	if (mode == NULL || (mode[0] != 'r' && mode[0] != 'w'))
	{
		errno = EINVAL;
		return NULL;
	}
	writing = mode[0] == 'w';
	for (slot = 0; slot < MAX_POPEN && popened[slot].f; slot++)
		;
	if (slot == MAX_POPEN)
	{
		errno = EMFILE;
		return NULL;
	}
	if (pipe(fds) < 0)
		return NULL;

	/*
	 * Neither end may leak into a child: not ours into this command (it would
	 * never see EOF on a pipe it holds both ends of), nor either into another
	 * popen's or system's command. The fd map still gives this command its
	 * end as fd 0 or 1.
	 */
	if (fcntl(fds[0], F_SETFD, FD_CLOEXEC) < 0 ||
		fcntl(fds[1], F_SETFD, FD_CLOEXEC) < 0)
	{
		save = errno;
		close(fds[0]);
		close(fds[1]);
		errno = save;
		return NULL;
	}
	if (fds[0] <= 2 || fds[1] <= 2)
	{
		/* stdio was closed: the child would inherit our own pipe end */
		close(fds[0]);
		close(fds[1]);
		errno = EMFILE;
		return NULL;
	}
	keep = writing ? fds[1] : fds[0];
	give = writing ? fds[0] : fds[1];
	pid = spawn_command(command, writing ? 0 : 1, give);
	save = errno;
	close(give);
	if (pid < 0)
	{
		close(keep);
		errno = save;
		return NULL;
	}
	f = fdopen(keep, writing ? "w" : "r");
	if (f == NULL)
	{
		save = errno;
		close(keep);
		wait_for(pid);
		errno = save;
		return NULL;
	}
	popened[slot].f = f;
	popened[slot].pid = pid;
	return f;
}

int
pclose(FILE *stream)
{
	int			slot;
	pid_t		pid;

	for (slot = 0; slot < MAX_POPEN && popened[slot].f != stream; slot++)
		;
	if (stream == NULL || slot == MAX_POPEN)
	{
		errno = ECHILD;
		return -1;
	}
	pid = popened[slot].pid;
	popened[slot].f = NULL;
	fclose(stream);
	return wait_for(pid);
}

int
system(const char *command)
{
	pid_t		pid;

	if (command == NULL)
		return 1;				/* a "shell" is available */
	pid = spawn_command(command, -1, -1);
	if (pid < 0)
		return -1;
	return wait_for(pid);
}
