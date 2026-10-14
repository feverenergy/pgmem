/* Name resolution. pgmem has only Unix-domain sockets; lookups always fail. */
#ifndef PGMEM_NETDB_H
#define PGMEM_NETDB_H
#include <netinet/in.h>
#include <sys/socket.h>
struct addrinfo
{
	int			ai_flags;
	int			ai_family;
	int			ai_socktype;
	int			ai_protocol;
	socklen_t	ai_addrlen;
	struct sockaddr *ai_addr;
	char	   *ai_canonname;
	struct addrinfo *ai_next;
};
#define AI_PASSIVE 0x01
#define AI_CANONNAME 0x02
#define AI_NUMERICHOST 0x04
#define AI_NUMERICSERV 0x400
#define NI_NUMERICHOST 0x01
#define NI_NUMERICSERV 0x02
#define NI_NAMEREQD 0x08
#define NI_MAXHOST 1025
#define NI_MAXSERV 32
#define EAI_BADFLAGS -1
#define EAI_NONAME -2
#define EAI_AGAIN -3
#define EAI_FAIL -4
#define EAI_FAMILY -6
#define EAI_SOCKTYPE -7
#define EAI_SERVICE -8
#define EAI_MEMORY -10
#define EAI_SYSTEM -11
#define EAI_OVERFLOW -12
int			getaddrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **res);
void		freeaddrinfo(struct addrinfo *res);
const char *gai_strerror(int errcode);
int			getnameinfo(const struct sockaddr *addr, socklen_t alen, char *host, socklen_t hlen, char *serv, socklen_t slen, int flags);
#endif
