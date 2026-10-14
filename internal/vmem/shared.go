//go:build darwin || linux

package vmem

import (
	"errors"
	"fmt"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PageAlign is the alignment of every overlay's offset and length. It is
// the largest host page size pgmem runs on, 16 KiB on macOS arm64, so
// overlays line up with pages on every host.
const PageAlign = 16 << 10

var (
	ErrNotVmem   = errors.New("vmem: not a live memory of this allocator")
	ErrAlignment = errors.New("vmem: offset or length is not a positive multiple of PageAlign")
	ErrRange     = errors.New("vmem: range outside the memory or the object, or no overlay there")
	ErrOverlap   = errors.New("vmem: range overlaps an overlay")
)

// Shared is an OS shared-memory object that Overlay maps into memories, so
// they all see the same bytes. Its pages start zero.
//
// Close must not run concurrently with an Overlay of the same object.
// Callers serialise the two. The kernel does so under its segment table's
// lock.
type Shared struct {
	fd   int
	size uint64
}

// maxShared is the largest size NewShared accepts. It is the largest
// multiple of PageAlign that fits the OS's int64 file size.
const maxShared = math.MaxInt64 &^ (PageAlign - 1)

// NewShared creates a shared object of size bytes rounded up to PageAlign.
func NewShared(size uint64) (*Shared, error) {
	if size == 0 || size > maxShared {
		return nil, ErrRange
	}
	// An overlay replaces whole host pages. A host page larger than
	// PageAlign, as on arm64 Linux kernels with 64 KiB pages, would let one
	// overlay cover its neighbour's bytes or the memory's private pages.
	// Refuse rather than map wrong.
	if ps := os.Getpagesize(); ps > PageAlign {
		return nil, fmt.Errorf("vmem: host page size %d exceeds PageAlign %d; shared memory is unsupported on this host", ps, PageAlign)
	}
	size = (size + PageAlign - 1) &^ (PageAlign - 1)
	fd, err := newSharedFD(int64(size))
	if err != nil {
		return nil, err
	}
	return &Shared{fd: fd, size: size}, nil
}

// Size is the object's size in bytes, a multiple of PageAlign.
func (s *Shared) Size() uint64 { return s.size }

// Close releases the object's descriptor. Mappings already made keep the
// pages until they are restored or their memory is freed.
func (s *Shared) Close() error {
	if s.fd < 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}

// lookup finds the live memory whose bytes start where mem does.
func (a *Allocator) lookup(mem []byte) (*memory, error) {
	if len(mem) == 0 {
		return nil, ErrNotVmem
	}
	a.mu.Lock()
	m := a.mems[uintptr(unsafe.Pointer(unsafe.SliceData(mem)))]
	a.mu.Unlock()
	if m == nil {
		return nil, ErrNotVmem
	}
	return m, nil
}

// Overlay maps the first length bytes of s over mem[off:off+length]. mem
// is a whole linear memory from this allocator, as api.Memory.Read(0,
// Size()) returns it. The range must lie within the memory's current size
// and not overlap another overlay. off and length are multiples of
// PageAlign.
//
// If the mapping itself fails, the range's previous contents are lost, and
// it holds private zero pages.
func (a *Allocator) Overlay(mem []byte, off uint64, s *Shared, length uint64) error {
	m, err := a.lookup(mem)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buf == nil {
		return ErrNotVmem
	}
	if length == 0 || off%PageAlign != 0 || length%PageAlign != 0 {
		return ErrAlignment
	}
	if off+length < off || off+length > m.size || length > s.size || s.fd < 0 {
		return ErrRange
	}
	for o, l := range m.overlays {
		if off < o+l && o < off+length {
			return ErrOverlap
		}
	}
	addr := unsafe.Pointer(&m.buf[off])
	got, err := unix.MmapPtr(s.fd, 0, addr, uintptr(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED)
	if err != nil {
		// A failed MAP_FIXED may have unmapped the range. Put private
		// pages back so the guest never faults there. If even that
		// fails, a hole inside the memory's bounds would fault the whole
		// process on the guest's next access, so stop here instead.
		if perr := m.private(off, length); perr != nil {
			panic(fmt.Sprintf("vmem: overlay failed (%v) and private pages could not be restored: %v", err, perr))
		}
		return err
	}
	if got != addr {
		panic("vmem: MAP_FIXED mapped elsewhere")
	}
	if m.overlays == nil {
		m.overlays = map[uint64]uint64{}
	}
	m.overlays[off] = length
	a.overlays.Add(1)
	return nil
}

// Restore replaces the overlay that starts at off with private zero pages,
// read-write like the rest of the memory.
func (a *Allocator) Restore(mem []byte, off uint64) error {
	m, err := a.lookup(mem)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	length, ok := m.overlays[off]
	if m.buf == nil || !ok {
		return ErrRange
	}
	// A failed MAP_FIXED leaves the range in an unknown state. It may be
	// unmapped, inside the memory's bounds. Stop rather than let the guest
	// fault the whole process.
	if err := m.private(off, length); err != nil {
		panic(fmt.Sprintf("vmem: restoring private pages: %v", err))
	}
	delete(m.overlays, off)
	a.overlays.Add(-1)
	return nil
}

// private maps private, zero, read-write pages over buf[off:off+length].
func (m *memory) private(off, length uint64) error {
	_, err := unix.MmapPtr(-1, 0, unsafe.Pointer(&m.buf[off]), uintptr(length),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_FIXED)
	return err
}
