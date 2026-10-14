/* File-system behaviour pgmem's WASI layer must provide, checked from C. */
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define CHECK(cond) do { if (!(cond)) { printf("FAIL %s:%d %s errno=%d\n", __FILE__, __LINE__, #cond, errno); return 1; } } while (0)

static int
write_file(const char *path, const char *text)
{
	int			fd = open(path, O_CREAT | O_WRONLY | O_TRUNC, 0600);

	if (fd < 0)
		return -1;
	if (write(fd, text, strlen(text)) != (ssize_t) strlen(text))
		return -1;
	return close(fd);
}

int
main(void)
{
	char		buf[256];
	struct stat st;
	int			fd;

	CHECK(mkdir("/t", 0700) == 0);
	CHECK(mkdir("/t", 0700) == -1 && errno == EEXIST);
	CHECK(chdir("/t") == 0);

	/* create, write and read back through a relative path */
	fd = open("a", O_CREAT | O_EXCL | O_RDWR, 0600);
	CHECK(fd >= 0);
	CHECK(write(fd, "hello world", 11) == 11);
	CHECK(lseek(fd, 0, SEEK_SET) == 0);
	CHECK(read(fd, buf, sizeof(buf)) == 11 && memcmp(buf, "hello world", 11) == 0);
	CHECK(read(fd, buf, sizeof(buf)) == 0);
	CHECK(pwrite(fd, "W", 1, 6) == 1);
	CHECK(pread(fd, buf, 5, 6) == 5 && memcmp(buf, "World", 5) == 0);
	CHECK(fstat(fd, &st) == 0 && S_ISREG(st.st_mode) && st.st_size == 11);
	CHECK(ftruncate(fd, 4) == 0 && fstat(fd, &st) == 0 && st.st_size == 4);
	CHECK(lseek(fd, 0, SEEK_END) == 4);
	CHECK(posix_fallocate(fd, 0, 100) == 0 && fstat(fd, &st) == 0 && st.st_size == 100);
	CHECK(ftruncate(fd, 4) == 0);
	CHECK(fsync(fd) == 0 && fdatasync(fd) == 0);
	CHECK(close(fd) == 0);
	CHECK(close(fd) == -1 && errno == EBADF);
	CHECK(open("a", O_CREAT | O_EXCL | O_RDWR, 0600) == -1 && errno == EEXIST);
	CHECK(open("missing", O_RDONLY) == -1 && errno == ENOENT);

	/* O_APPEND writes at the end whatever the offset */
	fd = open("a", O_WRONLY | O_APPEND);
	CHECK(fd >= 0);
	CHECK(lseek(fd, 0, SEEK_SET) == 0);
	CHECK(write(fd, "!", 1) == 1);
	CHECK(close(fd) == 0);
	CHECK(stat("/t/a", &st) == 0 && st.st_size == 5);

	/* O_TRUNC */
	CHECK(write_file("b", "xyz") == 0);
	fd = open("b", O_WRONLY | O_TRUNC);
	CHECK(fd >= 0 && close(fd) == 0);
	CHECK(stat("b", &st) == 0 && st.st_size == 0);

	/* rename over an existing file replaces it */
	CHECK(write_file("c", "new") == 0);
	CHECK(rename("c", "a") == 0);
	CHECK(stat("c", &st) == -1 && errno == ENOENT);
	CHECK(stat("a", &st) == 0 && st.st_size == 3);

	/* hard links share data and count links */
	CHECK(link("a", "a2") == 0);
	CHECK(stat("a", &st) == 0 && st.st_nlink == 2);
	CHECK(unlink("a") == 0);
	CHECK(stat("a2", &st) == 0 && st.st_nlink == 1 && st.st_size == 3);

	/* symlinks: stat follows, lstat and readlink do not */
	CHECK(symlink("a2", "s") == 0);
	CHECK(stat("s", &st) == 0 && S_ISREG(st.st_mode) && st.st_size == 3);
	CHECK(lstat("s", &st) == 0 && S_ISLNK(st.st_mode));
	CHECK(readlink("s", buf, sizeof(buf)) == 2 && memcmp(buf, "a2", 2) == 0);
	CHECK(readlink("a2", buf, sizeof(buf)) == -1 && errno == EINVAL);

	/* directories */
	CHECK(mkdir("d", 0700) == 0);
	CHECK(write_file("d/f", "x") == 0);
	CHECK(rmdir("d") == -1 && errno == ENOTEMPTY);
	CHECK(open("a2", O_RDONLY | O_DIRECTORY) == -1 && errno == ENOTDIR);
	CHECK(unlink("d/f") == 0 && rmdir("d") == 0);

	/* readdir lists ".", "..", then the names in order */
	{
		DIR		   *dir = opendir("/t");
		struct dirent *de;
		char		names[256] = "";

		CHECK(dir != NULL);
		while ((de = readdir(dir)) != NULL)
		{
			strcat(names, de->d_name);
			strcat(names, ",");
		}
		CHECK(closedir(dir) == 0);
		CHECK(strcmp(names, ".,..,a2,b,s,") == 0);
	}

	/* realpath walks readlink, which must say EINVAL for non-links */
	{
		char		rp[PATH_MAX];

		CHECK(realpath("/t/s", rp) != NULL && strcmp(rp, "/t/a2") == 0);
	}

	/* /dev/null */
	fd = open("/dev/null", O_RDWR);
	CHECK(fd >= 0);
	CHECK(write(fd, "x", 1) == 1);
	CHECK(read(fd, buf, 1) == 0);
	CHECK(close(fd) == 0);

	/* utimes */
	{
		struct timespec ts[2] = {{.tv_sec = 1000}, {.tv_sec = 2000}};

		CHECK(utimensat(AT_FDCWD, "b", ts, 0) == 0);
		CHECK(stat("b", &st) == 0 && st.st_mtim.tv_sec == 2000);
	}

	/* args and environment arrive */
	CHECK(getenv("PGMEM_FIXTURE") != NULL && strcmp(getenv("PGMEM_FIXTURE"), "yes") == 0);

	puts("fsops ok");
	return 0;
}
