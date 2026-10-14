/*
 * popen, pclose and system under the pgmem kernel, with the program
 * registered at /fixtures/shell. Without arguments it runs the checks and
 * prints "shell ok"; the other modes are the children it starts.
 */
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)
#define SELF "/fixtures/shell"

static int
child(int argc, char **argv)
{
	char		buf[256];
	ssize_t		n;
	long		total = 0;

	if (strcmp(argv[1], "args") == 0)
	{
		for (int i = 2; i < argc; i++)
			printf("%s|", argv[i]);
		return 0;
	}
	if (strcmp(argv[1], "count") == 0)
	{
		while ((n = read(0, buf, sizeof(buf))) > 0)
			total += n;
		return (int) (total % 256);
	}
	if (strcmp(argv[1], "noisy") == 0)
	{
		puts("noise on stdout");
		fputs("noise on stderr\n", stderr);
		return 3;
	}
	return 99;
}

int
main(int argc, char **argv)
{
	FILE	   *f,
			   *g;
	char		buf[256];
	int			st;

	if (argc > 1)
		return child(argc, argv);

	/* words, single and double quotes, and backslash escapes */
	f = popen("\"" SELF "\" args plain 'single quoted' \"double \\\"q\\\"\" back\\ slash", "r");
	CHECK(f != NULL);
	CHECK(fgets(buf, sizeof(buf), f) != NULL);
	CHECK(strcmp(buf, "plain|single quoted|double \"q\"|back slash|") == 0);
	st = pclose(f);
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0);

	/*
	 * Writing: the child sees EOF only at pclose. A second popen'd child
	 * must not inherit the first one's pipe, or the first never sees EOF.
	 */
	f = popen(SELF " count", "w");
	CHECK(f != NULL);
	CHECK(fputs("12345", f) >= 0);
	g = popen(SELF " count", "w");
	CHECK(g != NULL);
	st = pclose(f);
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 5);
	CHECK(fputs("xy", g) >= 0);
	st = pclose(g);
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 2);

	/* system with the redirections initdb uses; nothing reaches our stdout */
	st = system(SELF " noisy < /dev/null > /dev/null 2>&1");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 3);
	st = system("\"" SELF "\" noisy >/dev/null 2>/dev/null");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 3);

	/* shell features we do not implement fail instead of misbehaving */
	CHECK(popen(SELF " args a | cat", "r") == NULL && errno == EINVAL);
	CHECK(system(SELF " args $HOME") == -1 && errno == EINVAL);
	CHECK(system(SELF " args \"$HOME\"") == -1 && errno == EINVAL);
	CHECK(system(SELF " noisy > /tmp/out") == -1 && errno == ENOTSUP);
	CHECK(system(SELF " args 'unterminated") == -1 && errno == EINVAL);
	CHECK(system("   ") == -1 && errno == EINVAL);
	CHECK(popen("/fixtures/nope", "r") == NULL && errno == ENOENT);
	CHECK(popen(SELF, "x") == NULL && errno == EINVAL);
	CHECK(pclose(stdout) == -1 && errno == ECHILD);
	CHECK(system(NULL) == 1);

	/* initdb's exact form, quoted redirection targets */
	st = system("\"" SELF "\" noisy < \"/dev/null\" > \"/dev/null\" 2>&1");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 3);

	/* more shell syntax that must fail closed, not run something else */
	CHECK(system(SELF " noisy\n" SELF " noisy") == -1 && errno == EINVAL);
	CHECK(system(SELF " args a#b") == -1 && errno == EINVAL);
	CHECK(system(SELF " args a # comment") == -1 && errno == EINVAL);
	CHECK(system(SELF " noisy 0>/dev/null") == -1 && errno == EINVAL);
	CHECK(system(SELF " noisy 12>/dev/null") == -1 && errno == EINVAL);
	CHECK(system(SELF " noisy 2</dev/null") == -1 && errno == EINVAL);
	CHECK(system(SELF " noisy >/dev/null 2>&10") == -1 && errno == EINVAL);
	CHECK(system(SELF " noisy >/dev/null 2>&1x") == -1 && errno == EINVAL);
	CHECK(system(SELF " args a\\\nb") == -1 && errno == EINVAL);
	CHECK(system(SELF " args \"a\\\nb\"") == -1 && errno == EINVAL);

	puts("shell ok");
	return 0;
}
