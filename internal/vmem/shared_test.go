package vmem_test

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/feverenergy/pgmem/internal/vmem"
)

// newMem allocates a memory of size bytes, with a 64 MiB reservation, and
// frees it at cleanup.
func newMem(t *testing.T, a *vmem.Allocator, size uint64) []byte {
	t.Helper()
	m := a.Allocate(0, 64<<20)
	t.Cleanup(m.Free)
	b := m.Reallocate(size)
	if b == nil {
		t.Fatal("reallocate failed")
	}
	return b
}

func newShared(t *testing.T, size uint64) *vmem.Shared {
	t.Helper()
	s, err := vmem.NewShared(size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNewSharedRoundsUp(t *testing.T) {
	s := newShared(t, 1)
	if s.Size() != vmem.PageAlign {
		t.Fatalf("size %d", s.Size())
	}
	s2 := newShared(t, vmem.PageAlign+1)
	if s2.Size() != 2*vmem.PageAlign {
		t.Fatalf("size %d", s2.Size())
	}
	if _, err := vmem.NewShared(0); err == nil {
		t.Fatal("NewShared(0) succeeded")
	}
}

// NewShared rejects a size whose round-up to PageAlign does not fit an
// int64, the OS's file size. It does not wrap it to a small or negative
// size.
func TestNewSharedRejectsOverflow(t *testing.T) {
	for _, size := range []uint64{
		math.MaxInt64&^(vmem.PageAlign-1) + 1,
		math.MaxInt64,
		math.MaxUint64 - vmem.PageAlign + 2,
		math.MaxUint64,
	} {
		s, err := vmem.NewShared(size)
		if err == nil {
			s.Close()
		}
		if !errors.Is(err, vmem.ErrRange) {
			t.Errorf("NewShared(%#x) = %v, want ErrRange", size, err)
		}
	}
}

func TestOverlaySharesBytes(t *testing.T) {
	var a vmem.Allocator
	ma, mb := newMem(t, &a, 1<<20), newMem(t, &a, 1<<20)
	s := newShared(t, 64<<10)
	const off = 256 << 10
	for _, m := range [][]byte{ma, mb} {
		if err := a.Overlay(m, off, s, s.Size()); err != nil {
			t.Fatal(err)
		}
	}
	if ma[off] != 0 || mb[off+s.Size()-1] != 0 {
		t.Fatal("fresh object not zero")
	}
	ma[off+10] = 42
	mb[off+s.Size()-1] = 7
	if mb[off+10] != 42 || ma[off+s.Size()-1] != 7 {
		t.Fatal("writes not shared")
	}
	// Bytes outside the overlay stay private.
	ma[off-1], ma[off+s.Size()] = 1, 2
	if mb[off-1] != 0 || mb[off+s.Size()] != 0 {
		t.Fatal("bytes outside the overlay shared")
	}
	if st := a.Stats(); st.Overlays != 2 {
		t.Fatalf("overlays %d", st.Overlays)
	}
}

func TestOverlayAtomicsExact(t *testing.T) {
	var a vmem.Allocator
	ma, mb := newMem(t, &a, 1<<20), newMem(t, &a, 1<<20)
	s := newShared(t, vmem.PageAlign)
	for _, m := range [][]byte{ma, mb} {
		if err := a.Overlay(m, 0, s, s.Size()); err != nil {
			t.Fatal(err)
		}
	}
	const n = 200000
	var wg sync.WaitGroup
	for _, m := range [][]byte{ma, mb} {
		p := (*uint32)(unsafe.Pointer(&m[64]))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range n {
				atomic.AddUint32(p, 1)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&ma[64]))); got != 2*n {
		t.Fatalf("count %d, want %d", got, 2*n)
	}
}

func TestRestoreGivesPrivateZeroes(t *testing.T) {
	var a vmem.Allocator
	ma, mb := newMem(t, &a, 1<<20), newMem(t, &a, 1<<20)
	s := newShared(t, vmem.PageAlign)
	for _, m := range [][]byte{ma, mb} {
		if err := a.Overlay(m, 0, s, s.Size()); err != nil {
			t.Fatal(err)
		}
	}
	ma[0] = 9
	if err := a.Restore(ma, 0); err != nil {
		t.Fatal(err)
	}
	if ma[0] != 0 {
		t.Fatal("restored page not zero")
	}
	ma[1] = 5 // writable, and private
	if mb[0] != 9 || mb[1] != 0 {
		t.Fatalf("other memory sees %d %d", mb[0], mb[1])
	}
	if err := a.Restore(ma, 0); !errors.Is(err, vmem.ErrRange) {
		t.Fatalf("second restore: %v", err)
	}
	if st := a.Stats(); st.Overlays != 1 {
		t.Fatalf("overlays %d", st.Overlays)
	}
}

// A memory that grows after an overlay keeps it, because growth only
// touches pages above the old size.
func TestOverlaySurvivesGrow(t *testing.T) {
	var a vmem.Allocator
	m := a.Allocate(0, 64<<20)
	defer m.Free()
	small := m.Reallocate(1 << 20)
	other := newMem(t, &a, 1<<20)
	s := newShared(t, vmem.PageAlign)
	if err := a.Overlay(small, 512<<10, s, s.Size()); err != nil {
		t.Fatal(err)
	}
	if err := a.Overlay(other, 0, s, s.Size()); err != nil {
		t.Fatal(err)
	}
	big := m.Reallocate(8 << 20)
	big[512<<10] = 77
	if other[0] != 77 {
		t.Fatal("overlay lost after growth")
	}
}

func TestOverlayErrors(t *testing.T) {
	var a vmem.Allocator
	m := newMem(t, &a, 1<<20)
	s := newShared(t, vmem.PageAlign)
	cases := []struct {
		name   string
		mem    []byte
		off    uint64
		length uint64
		want   error
	}{
		{"foreign slice", make([]byte, 1<<20), 0, vmem.PageAlign, vmem.ErrNotVmem},
		{"empty slice", nil, 0, vmem.PageAlign, vmem.ErrNotVmem},
		{"unaligned offset", m, 4096, vmem.PageAlign, vmem.ErrAlignment},
		{"unaligned length", m, 0, 4096, vmem.ErrAlignment},
		{"zero length", m, 0, 0, vmem.ErrAlignment},
		{"past the memory", m, 1 << 20, vmem.PageAlign, vmem.ErrRange},
		{"longer than the object", m, 0, 2 * vmem.PageAlign, vmem.ErrRange},
	}
	for _, c := range cases {
		if err := a.Overlay(c.mem, c.off, s, c.length); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := a.Overlay(m, 0, s, s.Size()); err != nil {
		t.Fatal(err)
	}
	if err := a.Overlay(m, 0, s, s.Size()); !errors.Is(err, vmem.ErrOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	if err := a.Restore(m, vmem.PageAlign); !errors.Is(err, vmem.ErrRange) {
		t.Fatalf("restore without overlay: %v", err)
	}
}

// Freeing a memory drops its overlays. The object lives on for the others,
// and closing the object keeps the pages of mappings that remain.
func TestFreeAndCloseKeepOtherMappings(t *testing.T) {
	var a vmem.Allocator
	m1 := a.Allocate(0, 64<<20)
	b1 := m1.Reallocate(1 << 20)
	b2 := newMem(t, &a, 1<<20)
	s, err := vmem.NewShared(vmem.PageAlign)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range [][]byte{b1, b2} {
		if err := a.Overlay(m, 0, s, s.Size()); err != nil {
			t.Fatal(err)
		}
	}
	b1[3] = 3
	m1.Free()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if b2[3] != 3 {
		t.Fatal("data lost")
	}
	b2[4] = 4
	if st := a.Stats(); st.Overlays != 1 || st.Live != 1 {
		t.Fatalf("stats %+v", st)
	}
	if err := a.Overlay(b1, 0, s, s.Size()); !errors.Is(err, vmem.ErrNotVmem) {
		t.Fatalf("overlay into freed memory: %v", err)
	}
}
