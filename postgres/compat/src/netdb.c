/* Name resolution: pgmem has only Unix-domain sockets, so lookups fail. */
#include <netdb.h>
#include <stdlib.h>

int
getaddrinfo(const char *node, const char *service,
			const struct addrinfo *hints, struct addrinfo **res)
{
	(void) node;
	(void) service;
	(void) hints;
	*res = NULL;
	return EAI_FAIL;
}

void
freeaddrinfo(struct addrinfo *res)
{
	while (res)
	{
		struct addrinfo *next = res->ai_next;

		free(res);
		res = next;
	}
}

const char *
gai_strerror(int errcode)
{
	switch (errcode)
	{
		case EAI_NONAME:
			return "Name does not resolve";
		case EAI_FAMILY:
			return "Address family not supported";
		case EAI_MEMORY:
			return "Out of memory";
		default:
			return "Name resolution is not available in pgmem";
	}
}

int
getnameinfo(const struct sockaddr *addr, socklen_t alen, char *host,
			socklen_t hlen, char *serv, socklen_t slen, int flags)
{
	(void) addr;
	(void) alen;
	(void) host;
	(void) hlen;
	(void) serv;
	(void) slen;
	(void) flags;
	return EAI_FAIL;
}
