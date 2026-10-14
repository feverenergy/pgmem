/* Replaces wasi-libc's rusage, which only has the two time fields. */
#ifndef __wasilibc___struct_rusage_h
#define __wasilibc___struct_rusage_h
#include <__struct_timeval.h>
struct rusage
{
	struct timeval ru_utime;
	struct timeval ru_stime;
	long		ru_maxrss, ru_ixrss, ru_idrss, ru_isrss, ru_minflt, ru_majflt,
				ru_nswap, ru_inblock, ru_oublock, ru_msgsnd, ru_msgrcv,
				ru_nsignals, ru_nvcsw, ru_nivcsw;
};
#endif
