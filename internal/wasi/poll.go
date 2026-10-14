package wasi

import (
	"math"
	"slices"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// pollOneoff implements poll_oneoff(in, out, nsubscriptions, *nevents). It
// waits until at least one subscription is ready and reports every ready
// one. Each round takes the wait channels of the subscribed files before
// checking readiness. It then waits on them and the earliest clock
// deadline with Proc.Wait. So a change between the check and the wait is
// never lost, and nothing spins. A deliverable signal or the end of the
// call's context ends the call with EINTR.
func pollOneoff(c *call) wasiabi.Errno {
	in, out, nsubs, neventsPtr := c.u32(0), c.u32(1), c.u32(2), c.u32(3)
	if nsubs == 0 {
		return wasiabi.EINVAL
	}
	if uint64(nsubs)*wasiabi.SizeSubscription > math.MaxUint32 {
		return wasiabi.EFAULT
	}
	t0 := time.Now()
	for {
		// This looks up memory every round, but holding the slices across
		// the wait would be as safe. vmem never moves a memory, so a slice
		// into it stays valid while the module lives. fd_read holds its
		// iovec slices across waits.
		subs, err := c.bytes(in, nsubs*wasiabi.SizeSubscription)
		if err != wasiabi.ESUCCESS {
			return err
		}
		changed := c.queues(subs)
		events, err := c.bytes(out, nsubs*wasiabi.SizeEvent)
		if err != wasiabi.ESUCCESS {
			return err
		}
		if _, err := c.bytes(neventsPtr, 4); err != wasiabi.ESUCCESS {
			return err
		}
		n, next := c.pollRound(subs, events, t0)
		if n > 0 {
			return c.putU32(neventsPtr, n)
		}
		if err := c.p.Wait(c.ctx, next, changed...); err != wasiabi.ESUCCESS {
			return err
		}
	}
}

// queues returns the wait channels of the files that fd subscriptions in
// subs name. The caller takes them before checking readiness. Each channel
// appears once. Both directions of a socket share one queue, as do a
// pipe's two ends, and Wait selects on up to three channels without
// reflection.
func (c *call) queues(subs []byte) []<-chan struct{} {
	var chans []<-chan struct{}
	for ; len(subs) > 0; subs = subs[wasiabi.SizeSubscription:] {
		s := subs[:wasiabi.SizeSubscription]
		if tag := s[8]; tag != wasiabi.EventtypeFdRead && tag != wasiabi.EventtypeFdWrite {
			continue
		}
		d, err := c.p.FDs.Get(int32(le.Uint32(s[16:])))
		if err != wasiabi.ESUCCESS {
			continue
		}
		if w, ok := d.File().(Waitable); ok {
			if ch := w.Changed(); !slices.Contains(chans, ch) {
				chans = append(chans, ch)
			}
		}
	}
	return chans
}

// pollRound writes an event to events for every ready subscription in subs
// and returns how many it wrote. If it wrote none, it also returns the
// earliest clock deadline, or zero if there is no clock subscription.
func (c *call) pollRound(subs, events []byte, t0 time.Time) (uint32, time.Time) {
	now := time.Now()
	var n uint32
	var next time.Time
	emit := func(userdata uint64, err wasiabi.Errno, tag uint8, nbytes uint64, flags uint16) {
		e := events[n*wasiabi.SizeEvent:][:wasiabi.SizeEvent]
		clear(e)
		le.PutUint64(e, userdata)
		le.PutUint16(e[8:], uint16(err))
		e[10] = tag
		le.PutUint64(e[16:], nbytes)
		le.PutUint16(e[24:], flags)
		n++
	}
	for len(subs) > 0 {
		s := subs[:wasiabi.SizeSubscription]
		subs = subs[wasiabi.SizeSubscription:]
		userdata, tag := le.Uint64(s), s[8]
		switch tag {
		case wasiabi.EventtypeClock:
			due := deadline(le.Uint32(s[16:]), le.Uint64(s[24:]), le.Uint16(s[40:]), t0)
			if !now.Before(due) {
				emit(userdata, wasiabi.ESUCCESS, tag, 0, 0)
			} else if next.IsZero() || due.Before(next) {
				next = due
			}
		case wasiabi.EventtypeFdRead, wasiabi.EventtypeFdWrite:
			d, err := c.p.FDs.Get(int32(le.Uint32(s[16:])))
			if err != wasiabi.ESUCCESS {
				emit(userdata, err, tag, 0, 0)
				continue
			}
			ready, nbytes, hangup := d.File().Ready(tag)
			if !ready {
				continue
			}
			var flags uint16
			if hangup {
				flags = wasiabi.EventHangup
			}
			emit(userdata, wasiabi.ESUCCESS, tag, nbytes, flags)
		default:
			emit(userdata, wasiabi.EINVAL, tag, 0, 0)
		}
	}
	return n, next
}

// deadline returns when a clock subscription is due. It converts an
// absolute timeout on the realtime or monotonic clock to a deadline on
// Go's monotonic clock. Any other timeout counts from the start of the
// call, t0.
func deadline(id uint32, timeout uint64, flags uint16, t0 time.Time) time.Time {
	d := int64(min(timeout, math.MaxInt64))
	if flags&wasiabi.SubclockAbstime != 0 {
		switch id {
		case wasiabi.ClockRealtime:
			d -= t0.UnixNano()
		case wasiabi.ClockMonotonic:
			d -= int64(t0.Sub(start))
		}
	}
	return t0.Add(time.Duration(d))
}
