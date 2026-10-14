/*
 * Replaces wasi-libc's 48-byte sockaddr_storage, which cannot hold a
 * sockaddr_un (110 bytes): Postgres copies client addresses into one.
 */
#ifndef __wasilibc___struct_sockaddr_storage_h
#define __wasilibc___struct_sockaddr_storage_h
#include <__typedef_sa_family_t.h>
struct sockaddr_storage
{
	__attribute__((aligned(__BIGGEST_ALIGNMENT__))) sa_family_t ss_family;
	char		__ss_data[126];
};
#endif
