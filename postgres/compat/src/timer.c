/*
 * Timers and sleeping.
 *
 * setitimer(ITIMER_REAL) arms the process's one real-time timer in the
 * pgmem kernel, which delivers SIGALRM when it expires and re-arms it for
 * a non-zero interval. The old value it reports is the value last set, not
 * the time left.
 *
 * clock_nanosleep replaces wasi-libc's, which turns every poll_oneoff error
 * into ENOTSUP: here a sleep a signal interrupts fails with EINTR, as
 * nanosleep, usleep and sleep (wasi-libc's, built on this one) promise.
 */
#include <errno.h>
#include <stdint.h>
#include <string.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>
#include <wasi/api.h>

#define PGM_IMPORT(name) __attribute__((import_module("pgm"), import_name(name)))

PGM_IMPORT("setitimer") int32_t pgm_setitimer(uint32_t value_lo, uint32_t value_hi,
											  uint32_t interval_lo, uint32_t interval_hi);

static struct itimerval real_timer;

/* tv in microseconds; false if tv_usec is out of range or tv negative. */
static int
timeval_usec(const struct timeval *tv, uint64_t *usec)
{
	if (tv->tv_sec < 0 || tv->tv_usec < 0 || tv->tv_usec >= 1000000)
		return 0;
	if ((uint64_t) tv->tv_sec > (UINT64_MAX - 999999) / 1000000)
		*usec = UINT64_MAX;
	else
		*usec = (uint64_t) tv->tv_sec * 1000000 + (uint64_t) tv->tv_usec;
	return 1;
}

int
setitimer(int which, const struct itimerval *value, struct itimerval *ovalue)
{
	uint64_t	v,
				i;
	int32_t		r;

	if (which != ITIMER_REAL)
	{
		errno = EINVAL;
		return -1;
	}
	if (value)
	{
		if (!timeval_usec(&value->it_value, &v) ||
			!timeval_usec(&value->it_interval, &i))
		{
			errno = EINVAL;
			return -1;
		}
		r = pgm_setitimer((uint32_t) v, (uint32_t) (v >> 32),
						  (uint32_t) i, (uint32_t) (i >> 32));
		if (r < 0)
		{
			errno = -r;
			return -1;
		}
	}
	if (ovalue)
		*ovalue = real_timer;
	if (value)
		real_timer = *value;
	return 0;
}

unsigned
alarm(unsigned seconds)
{
	struct itimerval v;
	struct itimerval old;

	memset(&v, 0, sizeof(v));
	v.it_value.tv_sec = seconds;
	if (setitimer(ITIMER_REAL, &v, &old) < 0)
		return 0;
	return old.it_value.tv_sec + (old.it_value.tv_usec > 0);
}

int
clock_nanosleep(clockid_t clock_id, int flags, const struct timespec *rqtp,
				struct timespec *rmtp)
{
	__wasi_subscription_t sub;
	__wasi_event_t ev;
	__wasi_size_t n;
	__wasi_errno_t err;
	struct timespec t0;
	uint64_t	ns;

	if ((flags & ~TIMER_ABSTIME) != 0 ||
		rqtp->tv_nsec < 0 || rqtp->tv_nsec >= 1000000000)
		return EINVAL;
	if (clock_id != CLOCK_REALTIME && clock_id != CLOCK_MONOTONIC)
		return ENOTSUP;
	if (rqtp->tv_sec < 0)
		ns = 0;
	else if ((uint64_t) rqtp->tv_sec > (UINT64_MAX - 999999999) / 1000000000)
		ns = UINT64_MAX;
	else
		ns = (uint64_t) rqtp->tv_sec * 1000000000 + (uint64_t) rqtp->tv_nsec;

	memset(&sub, 0, sizeof(sub));
	sub.u.tag = __WASI_EVENTTYPE_CLOCK;
	sub.u.u.clock.id = clock_id == CLOCK_MONOTONIC ?
		__WASI_CLOCKID_MONOTONIC : __WASI_CLOCKID_REALTIME;
	sub.u.u.clock.timeout = ns;
	if (flags & TIMER_ABSTIME)
		sub.u.u.clock.flags = __WASI_SUBCLOCKFLAGS_SUBSCRIPTION_CLOCK_ABSTIME;
	clock_gettime(CLOCK_MONOTONIC, &t0);
	err = __wasi_poll_oneoff(&sub, &ev, 1, &n);
	if (err == 0)
		err = ev.error;
	if (err == __WASI_ERRNO_INTR && rmtp && !(flags & TIMER_ABSTIME))
	{
		struct timespec t1;
		int64_t		left;

		clock_gettime(CLOCK_MONOTONIC, &t1);
		left = (int64_t) (ns > INT64_MAX ? INT64_MAX : ns) -
			((int64_t) (t1.tv_sec - t0.tv_sec) * 1000000000 + (t1.tv_nsec - t0.tv_nsec));
		if (left < 0)
			left = 0;
		rmtp->tv_sec = left / 1000000000;
		rmtp->tv_nsec = left % 1000000000;
	}
	return err;
}
