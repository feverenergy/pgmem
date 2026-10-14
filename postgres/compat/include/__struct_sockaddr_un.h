/* Replaces wasi-libc's sockaddr_un, which has no sun_path. */
#ifndef __wasilibc___struct_sockaddr_un_h
#define __wasilibc___struct_sockaddr_un_h
#include <__typedef_sa_family_t.h>
struct sockaddr_un
{
	sa_family_t sun_family;
	char		sun_path[108];
};
#endif
