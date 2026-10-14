package wasi

import (
	"context"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// Guest memory used by the poll_oneoff tests, beside hostProc's data.
const (
	subsAt    = 400 // up to 4 subscriptions
	eventsAt  = 600 // up to 3 events
	neventsAt = 700
)

type sub struct {
	userdata uint64
	tag      uint8
	fd       uint32 // fd subscriptions
	clock    uint32 // clock subscriptions, with id, timeout and flags
	timeout  uint64
	flags    uint16
}

type event struct {
	userdata uint64
	errno    wasiabi.Errno
	tag      uint8
	nbytes   uint64
	flags    uint16
}

func putSubs(mem *fakeMem, subs ...sub) {
	for i, s := range subs {
		b := mem.b[subsAt+i*wasiabi.SizeSubscription:][:wasiabi.SizeSubscription]
		clear(b)
		le.PutUint64(b, s.userdata)
		b[8] = s.tag
		if s.tag == wasiabi.EventtypeClock {
			le.PutUint32(b[16:], s.clock)
			le.PutUint64(b[24:], s.timeout)
			le.PutUint16(b[40:], s.flags)
		} else {
			le.PutUint32(b[16:], s.fd)
		}
	}
}

func getEvents(mem *fakeMem) []event {
	evs := make([]event, le.Uint32(mem.b[neventsAt:]))
	for i := range evs {
		b := mem.b[eventsAt+i*wasiabi.SizeEvent:]
		evs[i] = event{le.Uint64(b), wasiabi.Errno(le.Uint16(b[8:])), b[10], le.Uint64(b[16:]), le.Uint16(b[24:])}
	}
	return evs
}

// pollProc returns hostProc's process with a pipe. Its read end is fd 5
// and its write end fd 6.
func pollProc() (*Proc, *fakeMem, File, File) {
	p, mem := hostProc()
	r, w := NewPipe()
	p.FDs.InstallAt(5, NewDesc(r, 0))
	p.FDs.InstallAt(6, NewDesc(w, 0))
	return p, mem, r, w
}

func poll(p *Proc, mem *fakeMem, nsubs uint64) wasiabi.Errno {
	return invoke(p, mem, pollOneoff, subsAt, eventsAt, nsubs, neventsAt)
}

func TestPollOneoffArguments(t *testing.T) {
	p, mem, _, _ := pollProc()
	putSubs(mem, sub{tag: wasiabi.EventtypeFdRead, fd: 4})
	cases := []struct {
		name string
		args []uint64
		want wasiabi.Errno
	}{
		{"no subscriptions", []uint64{subsAt, eventsAt, 0, neventsAt}, wasiabi.EINVAL},
		{"subscriptions", []uint64{outOfRange, eventsAt, 1, neventsAt}, wasiabi.EFAULT},
		{"subscription count", []uint64{subsAt, eventsAt, 1 << 31, neventsAt}, wasiabi.EFAULT},
		{"events", []uint64{subsAt, memSize - 8, 1, neventsAt}, wasiabi.EFAULT},
		{"nevents", []uint64{subsAt, eventsAt, 1, memSize - 2}, wasiabi.EFAULT},
	}
	for _, tc := range cases {
		if got := invoke(p, mem, pollOneoff, tc.args...); got != tc.want {
			t.Errorf("%s: errno %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPollOneoffImmediateEvents(t *testing.T) {
	p, mem, _, _ := pollProc()
	putSubs(mem,
		sub{userdata: 1, tag: wasiabi.EventtypeFdRead, fd: 9},  // not open
		sub{userdata: 2, tag: wasiabi.EventtypeFdRead, fd: 5},  // empty pipe
		sub{userdata: 3, tag: wasiabi.EventtypeFdWrite, fd: 4}, // /dev/zero
	)
	if err := poll(p, mem, 3); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	want := []event{
		{userdata: 1, errno: wasiabi.EBADF, tag: wasiabi.EventtypeFdRead},
		{userdata: 3, tag: wasiabi.EventtypeFdWrite},
	}
	if got := getEvents(mem); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events %+v, want %+v", got, want)
	}

	// Absolute deadlines in the past are due at once.
	putSubs(mem,
		sub{userdata: 4, tag: wasiabi.EventtypeClock, clock: wasiabi.ClockMonotonic, timeout: 1, flags: wasiabi.SubclockAbstime},
		sub{userdata: 5, tag: wasiabi.EventtypeClock, clock: wasiabi.ClockRealtime, timeout: 1, flags: wasiabi.SubclockAbstime},
		sub{userdata: 6, tag: wasiabi.EventtypeClock, clock: wasiabi.ClockMonotonic, timeout: 1 << 63},
	)
	if err := poll(p, mem, 3); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	if got := getEvents(mem); len(got) != 2 || got[0].userdata != 4 || got[1].userdata != 5 {
		t.Fatalf("events %+v", got)
	}
}

func TestPollOneoffTimeout(t *testing.T) {
	p, mem, _, _ := pollProc()
	putSubs(mem,
		sub{userdata: 1, tag: wasiabi.EventtypeFdRead, fd: 5},
		sub{userdata: 2, tag: wasiabi.EventtypeClock, clock: wasiabi.ClockMonotonic, timeout: uint64(30 * time.Millisecond)},
	)
	t0 := time.Now()
	if err := poll(p, mem, 2); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 30*time.Millisecond {
		t.Fatalf("returned after %v", d)
	}
	if got := getEvents(mem); len(got) != 1 || got[0] != (event{userdata: 2, tag: wasiabi.EventtypeClock}) {
		t.Fatalf("events %+v", got)
	}
}

// A poll with no timeout wakes when the pipe gets data. It wakes again
// when the writer closes, as readable with hang-up.
func TestPollOneoffWakesOnPipe(t *testing.T) {
	p, mem, r, w := pollProc()
	putSubs(mem, sub{userdata: 7, tag: wasiabi.EventtypeFdRead, fd: 5})
	go func() {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("abc"), 0)
	}()
	if err := poll(p, mem, 1); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	if got := getEvents(mem); len(got) != 1 || got[0] != (event{userdata: 7, tag: wasiabi.EventtypeFdRead, nbytes: 3}) {
		t.Fatalf("events %+v", got)
	}
	r.Read(make([]byte, 3), 0)
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.FDs.Close(6)
	}()
	if err := poll(p, mem, 1); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	if got := getEvents(mem); len(got) != 1 || got[0] != (event{userdata: 7, tag: wasiabi.EventtypeFdRead, flags: wasiabi.EventHangup}) {
		t.Fatalf("events %+v", got)
	}
}

func TestPollOneoffInterrupted(t *testing.T) {
	p, mem, _, _ := pollProc()
	putSubs(mem, sub{tag: wasiabi.EventtypeFdRead, fd: 5})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	c := call{ctx: ctx, mem: mem, p: p, stack: []uint64{subsAt, eventsAt, 1, neventsAt}}
	if err := pollOneoff(&c); err != wasiabi.EINTR {
		t.Fatalf("errno %d, want EINTR", err)
	}
}

// A deliverable signal ends a blocked poll_oneoff with EINTR, and one
// already pending when the call starts does too.
func TestPollOneoffInterruptedBySignal(t *testing.T) {
	p, mem, _, _ := pollProc()
	sig := newTestSignals()
	p.Signals = sig
	putSubs(mem, sub{tag: wasiabi.EventtypeFdRead, fd: 5})
	time.AfterFunc(20*time.Millisecond, sig.send)
	if err := poll(p, mem, 1); err != wasiabi.EINTR {
		t.Fatalf("errno %d, want EINTR", err)
	}
	if err := poll(p, mem, 1); err != wasiabi.EINTR {
		t.Fatalf("pending signal: errno %d, want EINTR", err)
	}
}

func TestPollManyFDs(t *testing.T) {
	p, mem := hostProc()
	var ws []File
	var subs []sub
	for i := 0; i < 5; i++ {
		r, w := NewPipe()
		p.FDs.InstallAt(int32(10+2*i), NewDesc(r, 0))
		ws = append(ws, w)
		subs = append(subs, sub{userdata: uint64(100 + i), tag: wasiabi.EventtypeFdRead, fd: uint32(10 + 2*i)})
	}
	defer func() {
		for _, w := range ws {
			w.Close()
		}
	}()
	putSubs(mem, subs...)
	go func() {
		time.Sleep(10 * time.Millisecond)
		ws[3].Write([]byte("x"), 0)
	}()
	// Five subscriptions overflow the shared layout, so events go further
	// up.
	const evAt, nevAt = 2000, 2400
	mem.b = append(mem.b, make([]byte, 2048)...)
	done := make(chan wasiabi.Errno, 1)
	go func() { done <- invoke(p, mem, pollOneoff, subsAt, evAt, 5, nevAt) }()
	select {
	case err := <-done:
		if err != wasiabi.ESUCCESS {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll_oneoff still blocked after a write")
	}
	if n := le.Uint32(mem.b[nevAt:]); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
	if ud, tag := le.Uint64(mem.b[evAt:]), mem.b[evAt+10]; ud != 103 || tag != wasiabi.EventtypeFdRead {
		t.Fatalf("event userdata %d tag %d", ud, tag)
	}
}

// Subscriptions that share a wait queue contribute one channel. A
// backend polls its socket (POLLIN|POLLOUT), its latch and the
// postmaster-death pipe. That poll stays within the three channels Wait
// selects on without reflection.
func TestPollQueuesDeduplicated(t *testing.T) {
	p, mem, _, _ := pollProc()
	a, b := NewSockPair()
	defer b.Close()
	p.FDs.InstallAt(7, NewDesc(a, 0))
	r, w := NewPipe()
	defer w.Close()
	p.FDs.InstallAt(10, NewDesc(r, 0))
	putSubs(mem,
		sub{tag: wasiabi.EventtypeFdRead, fd: 7},
		sub{tag: wasiabi.EventtypeFdWrite, fd: 7},
		sub{tag: wasiabi.EventtypeFdRead, fd: 5},
		sub{tag: wasiabi.EventtypeFdRead, fd: 10},
	)
	c := call{ctx: context.Background(), mem: mem, p: p}
	if got := c.queues(mem.b[subsAt:][:4*wasiabi.SizeSubscription]); len(got) != 3 {
		t.Fatalf("%d channels, want 3", len(got))
	}
}
