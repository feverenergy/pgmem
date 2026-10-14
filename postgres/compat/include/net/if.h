#ifndef PGMEM_NET_IF_H
#define PGMEM_NET_IF_H
#define IFNAMSIZ 16
struct ifreq
{
	char		ifr_name[IFNAMSIZ];
};
#endif
