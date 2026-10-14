package wasi

import (
	"bytes"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// clockAt reads clock id through clock_time_get into guest memory at 0.
func clockAt(t *testing.T, p *Proc, mem *fakeMem, id uint32) int64 {
	t.Helper()
	if err := invoke(p, mem, clockTimeGet, uint64(id), 0, 0); err != wasiabi.ESUCCESS {
		t.Fatalf("clock_time_get(%d): errno %d", id, err)
	}
	return int64(le.Uint64(mem.b))
}

func TestRealtimeUsesProcClock(t *testing.T) {
	p, mem := hostProc()
	p.Realtime = func() int64 { return 42 }
	if got := clockAt(t, p, mem, wasiabi.ClockRealtime); got != 42 {
		t.Fatalf("realtime = %d, want 42", got)
	}
	// The monotonic clock stays real. It reads the time since the package
	// started.
	before := int64(time.Since(start))
	got := clockAt(t, p, mem, wasiabi.ClockMonotonic)
	if after := int64(time.Since(start)); got < before || got > after {
		t.Fatalf("monotonic = %d, want within [%d, %d]", got, before, after)
	}
}

func TestRealtimeDefaultsToHostClock(t *testing.T) {
	p, mem := hostProc()
	before := time.Now().UnixNano()
	got := clockAt(t, p, mem, wasiabi.ClockRealtime)
	if after := time.Now().UnixNano(); got < before || got > after {
		t.Fatalf("realtime = %d, want within [%d, %d]", got, before, after)
	}
}

func TestRandomUsesProcSource(t *testing.T) {
	p, mem := hostProc()
	want := []byte("0123456789abcdef")
	p.Random = bytes.NewReader(want)
	if err := invoke(p, mem, randomGet, 100, uint64(len(want))); err != wasiabi.ESUCCESS {
		t.Fatalf("random_get: errno %d", err)
	}
	if got := mem.b[100 : 100+len(want)]; !bytes.Equal(got, want) {
		t.Fatalf("random_get wrote %q, want %q", got, want)
	}
}

// /dev/urandom opened by a process with a Random source reads from it too.
// Postgres without OpenSSL takes its strong random bytes from there.
func TestURandomUsesProcSource(t *testing.T) {
	p, mem := hostProc()
	want := []byte("fedcba9876543210")
	p.Random = bytes.NewReader(want)
	const pathAt, fdAt, nreadAt = 400, 420, 424
	path := "dev/urandom"
	copy(mem.b[pathAt:], path)
	if err := invoke(p, mem, pathOpen, 3, 0, pathAt, uint64(len(path)), 0, 0, 0, 0, fdAt); err != wasiabi.ESUCCESS {
		t.Fatalf("path_open: errno %d", err)
	}
	fd := uint64(le.Uint32(mem.b[fdAt:]))
	// The iovec at 0 is the 16 bytes at 64.
	if err := invoke(p, mem, fdRead, fd, 0, 1, nreadAt); err != wasiabi.ESUCCESS {
		t.Fatalf("fd_read: errno %d", err)
	}
	if n := le.Uint32(mem.b[nreadAt:]); n != uint32(len(want)) {
		t.Fatalf("fd_read read %d bytes, want %d", n, len(want))
	}
	if got := mem.b[64 : 64+len(want)]; !bytes.Equal(got, want) {
		t.Fatalf("/dev/urandom gave %q, want %q", got, want)
	}
}
