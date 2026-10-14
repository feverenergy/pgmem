//go:build darwin || linux

// Package vmem backs WebAssembly linear memories with their own memory
// mappings. It reserves the maximum size as inaccessible address space up
// front, and pages become read-write as the memory grows. So the base
// address never moves, which wazero requires for shared memories, and
// freeing returns the pages to the OS at once. wazero's default allocates
// the declared maximum as one Go slice per instance. For pgmem's modules
// that maximum is 4 GiB.
package vmem

import (
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/tetratelabs/wazero/experimental"
)

// commitChunk is how much address space becomes read-write at a time.
const commitChunk = 1 << 20

// Allocator implements experimental.MemoryAllocator. The zero value is ready
// to use and safe for concurrent use.
type Allocator struct {
	live          atomic.Int64
	committed     atomic.Int64
	peakCommitted atomic.Int64
	overlays      atomic.Int64

	mu   sync.Mutex
	mems map[uintptr]*memory // live memories by base address, for Overlay
}

// Stats reports current and peak usage.
type Stats struct {
	Live          int64 // memories allocated and not yet freed
	Committed     int64 // bytes currently read-write
	PeakCommitted int64
	Overlays      int64 // shared mappings in live memories
}

// Stats returns a snapshot of the allocator's counters.
func (a *Allocator) Stats() Stats {
	return Stats{
		Live:          a.live.Load(),
		Committed:     a.committed.Load(),
		PeakCommitted: a.peakCommitted.Load(),
		Overlays:      a.overlays.Load(),
	}
}

// Allocate reserves max bytes of address space. It panics if the reservation
// fails, because wazero offers no error path here.
func (a *Allocator) Allocate(_, max uint64) experimental.LinearMemory {
	buf, err := syscall.Mmap(-1, 0, int(max), syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		panic("vmem: reserving " + err.Error())
	}
	m := &memory{a: a, buf: buf}
	a.mu.Lock()
	if a.mems == nil {
		a.mems = map[uintptr]*memory{}
	}
	a.mems[m.base()] = m
	a.mu.Unlock()
	a.live.Add(1)
	return m
}

type memory struct {
	a *Allocator

	mu        sync.Mutex
	buf       []byte // the whole reservation, or nil once freed
	committed uint64
	size      uint64            // the memory's size, from the last Reallocate
	overlays  map[uint64]uint64 // offset -> length of each shared mapping
}

func (m *memory) base() uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(m.buf))) }

// Reallocate makes the first size bytes read-write and returns them. It
// returns nil when size exceeds the reservation or the OS refuses. Pages
// below the old size, overlays among them, are left alone.
func (m *memory) Reallocate(size uint64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buf == nil || size > uint64(len(m.buf)) {
		return nil
	}
	if size > m.committed {
		end := min((size+commitChunk-1)&^(commitChunk-1), uint64(len(m.buf)))
		if err := syscall.Mprotect(m.buf[m.committed:end], syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			return nil
		}
		n := m.a.committed.Add(int64(end - m.committed))
		for p := m.a.peakCommitted.Load(); n > p && !m.a.peakCommitted.CompareAndSwap(p, n); p = m.a.peakCommitted.Load() {
		}
		m.committed = end
	}
	m.size = max(m.size, size)
	return m.buf[:size:m.committed]
}

// Free unmaps the reservation, overlays included. Calling it again is a
// no-op.
func (m *memory) Free() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buf == nil {
		return
	}
	m.a.mu.Lock()
	delete(m.a.mems, m.base())
	m.a.mu.Unlock()
	m.a.overlays.Add(-int64(len(m.overlays)))
	m.overlays = nil
	m.a.committed.Add(-int64(m.committed))
	m.a.live.Add(-1)
	_ = syscall.Munmap(m.buf)
	m.buf = nil
}
