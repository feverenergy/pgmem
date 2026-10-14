/*
 * The one user every pgmem process runs as. Postgres refuses to run as
 * root, so the uid is not 0.
 */
#include <errno.h>
#include <grp.h>
#include <pwd.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define PGMEM_UID 1000

static char user_name[] = "postgres";
static char home_dir[] = "/pgmem";
static char empty[] = "";
static struct passwd the_user = {
	user_name, empty, PGMEM_UID, PGMEM_UID, empty, home_dir, empty
};
static mode_t file_mask = 022;

uid_t
getuid(void)
{
	return PGMEM_UID;
}

uid_t
geteuid(void)
{
	return PGMEM_UID;
}

gid_t
getgid(void)
{
	return PGMEM_UID;
}

gid_t
getegid(void)
{
	return PGMEM_UID;
}

struct passwd *
getpwuid(uid_t uid)
{
	if (uid != PGMEM_UID)
	{
		errno = ENOENT;
		return NULL;
	}
	return &the_user;
}

struct passwd *
getpwnam(const char *name)
{
	if (strcmp(name, user_name) != 0)
	{
		errno = ENOENT;
		return NULL;
	}
	return &the_user;
}

int
getpwuid_r(uid_t uid, struct passwd *pwd, char *buf, size_t buflen,
		   struct passwd **result)
{
	(void) buf;
	(void) buflen;
	*result = NULL;
	if (uid != PGMEM_UID)
		return 0;
	*pwd = the_user;
	*result = pwd;
	return 0;
}

struct group *
getgrnam(const char *name)
{
	(void) name;
	return NULL;
}

struct group *
getgrgid(gid_t gid)
{
	(void) gid;
	return NULL;
}

mode_t
umask(mode_t mask)
{
	mode_t		old = file_mask;

	file_mask = mask & 0777;
	return old;
}

int
chown(const char *path, uid_t owner, gid_t group)
{
	(void) path;
	(void) owner;
	(void) group;
	return 0;
}

/* WASI has no permission bits: succeed when the file exists. */
int
pgmem_chmod(const char *path, mode_t mode)
{
	struct stat st;

	(void) mode;
	return stat(path, &st);
}

int
pgmem_fchmod(int fd, mode_t mode)
{
	struct stat st;

	(void) mode;
	return fstat(fd, &st);
}
