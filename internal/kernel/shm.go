package kernel

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// System V shared memory. Each segment is a vmem.Shared object. Attaching
// maps it over the attacher's linear memory at an address the guest chose,
// so every attacher sees the same bytes. postgres/compat/src/sysv.c picks
// a reserved range or a heap block for the address. Keys and ids are per
// kernel.

// shmget flags, as in compat's sys/ipc.h.
const (
	ipcCreat = 0o1000
	ipcExcl  = 0o2000
)

const (
	maxShmSize = 1 << 30 // bytes per segment
	maxShmSegs = 4096    // segments per kernel
)

type shmSeg struct {
	id, key    int32
	size       uint32 // as requested; the object is rounded up to vmem.PageAlign
	obj        *vmem.Shared
	nattch     int
	removed    bool // IPC_RMID ran, so the last detach frees it
	cpid, lpid int32
}

type shmAttach struct {
	seg  *shmSeg
	addr uint32
}

// shmStat is what shm_stat reports.
type shmStat struct {
	key                  int32 // 0 once removed, as on Linux
	size, mapped, nattch uint32
	cpid, lpid           int32
}

type shmTable struct {
	alloc *vmem.Allocator
	// onFree, if set, runs under mu when a segment is freed. The kernel's
	// semaphore table uses it to drop the semaphores that lived in the
	// segment.
	onFree func(id int32)
	// report, if set, receives a line for the process pid when a call of
	// pid's fails for a reason its errno does not say. It is called without
	// mu.
	report func(pid int32, msg string)
	// newShared makes a segment's object; tests replace it.
	newShared func(size uint64) (*vmem.Shared, error)

	mu       sync.Mutex
	closed   bool
	nextID   int32
	segs     map[int32]*shmSeg
	keys     map[int32]*shmSeg     // keyed segments not yet removed
	attached map[int32][]shmAttach // by pid
}

func newShmTable(a *vmem.Allocator) *shmTable {
	return &shmTable{
		alloc:     a,
		newShared: vmem.NewShared,
		nextID:    1,
		segs:      map[int32]*shmSeg{},
		keys:      map[int32]*shmSeg{},
		attached:  map[int32][]shmAttach{},
	}
}

// get is shmget. Key 0 (IPC_PRIVATE) always creates. If the segment's
// object cannot be made, report sends the reason to the caller's stderr.
func (t *shmTable) get(pid, key int32, size uint32, flags int32) (int32, wasiabi.Errno) {
	var failure error
	defer func() { // runs after mu is released
		if failure != nil && t.report != nil {
			t.report(pid, fmt.Sprintf("pgmem: shmget of %d bytes failed: %v\n", size, failure))
		}
	}()
	t.mu.Lock()
	defer t.mu.Unlock()
	if key != 0 {
		if s := t.keys[key]; s != nil {
			if flags&ipcCreat != 0 && flags&ipcExcl != 0 {
				return 0, wasiabi.EEXIST
			}
			if size > s.size {
				return 0, wasiabi.EINVAL
			}
			return s.id, wasiabi.ESUCCESS
		}
		if flags&ipcCreat == 0 {
			return 0, wasiabi.ENOENT
		}
	}
	if size == 0 || size > maxShmSize {
		return 0, wasiabi.EINVAL
	}
	if t.closed {
		return 0, wasiabi.ENOMEM
	}
	// Ids are never reused and must stay positive, because a negative
	// result is -errno. Once they run out, no segment is created again.
	if len(t.segs) >= maxShmSegs || t.nextID == math.MaxInt32 {
		return 0, wasiabi.ENOSPC
	}
	obj, err := t.newShared(uint64(size))
	if err != nil {
		failure = err
		return 0, wasiabi.ENOMEM
	}
	s := &shmSeg{id: t.nextID, key: key, size: size, obj: obj, cpid: pid}
	t.nextID++
	t.segs[s.id] = s
	if key != 0 {
		t.keys[key] = s
	}
	return s.id, wasiabi.ESUCCESS
}

// attach maps segment id over mem, pid's whole linear memory, at addr. It
// maps the whole object, whose size is rounded up to vmem.PageAlign.
func (t *shmTable) attach(pid int32, mem []byte, id int32, addr uint32) wasiabi.Errno {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.segs[id]
	if s == nil || addr == 0 || addr%vmem.PageAlign != 0 {
		return wasiabi.EINVAL
	}
	switch err := t.alloc.Overlay(mem, uint64(addr), s.obj, s.obj.Size()); {
	case err == nil:
	case errors.Is(err, vmem.ErrAlignment), errors.Is(err, vmem.ErrRange), errors.Is(err, vmem.ErrOverlap):
		return wasiabi.EINVAL
	default: // not a vmem memory, or the OS refused
		return wasiabi.ENOMEM
	}
	s.nattch++
	s.lpid = pid
	t.attached[pid] = append(t.attached[pid], shmAttach{s, addr})
	return wasiabi.ESUCCESS
}

// detach undoes pid's attachment at addr, putting private zero pages back.
func (t *shmTable) detach(pid int32, mem []byte, addr uint32) wasiabi.Errno {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.attached[pid]
	i := slices.IndexFunc(list, func(a shmAttach) bool { return a.addr == addr })
	if i < 0 {
		return wasiabi.EINVAL
	}
	if err := t.alloc.Restore(mem, uint64(addr)); err != nil {
		return wasiabi.EINVAL
	}
	s := list[i].seg
	if list = slices.Delete(list, i, i+1); len(list) == 0 {
		delete(t.attached, pid)
	} else {
		t.attached[pid] = list
	}
	s.nattch--
	s.lpid = pid
	t.maybeFree(s)
	return wasiabi.ESUCCESS
}

func (t *shmTable) stat(id int32) (shmStat, wasiabi.Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.segs[id]
	if s == nil {
		return shmStat{}, wasiabi.EINVAL
	}
	st := shmStat{key: s.key, size: s.size, mapped: uint32(s.obj.Size()), nattch: uint32(s.nattch), cpid: s.cpid, lpid: s.lpid}
	if s.removed {
		st.key = 0
	}
	return st, wasiabi.ESUCCESS
}

// rmid is IPC_RMID. The key goes at once, and the segment goes at its last
// detach.
func (t *shmTable) rmid(id int32) wasiabi.Errno {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.segs[id]
	if s == nil {
		return wasiabi.EINVAL
	}
	s.removed = true
	if t.keys[s.key] == s {
		delete(t.keys, s.key)
	}
	t.maybeFree(s)
	return wasiabi.ESUCCESS
}

// release drops every attachment of an exiting process. It only updates
// the bookkeeping. The process's memory is freed, or is about to be, with
// its mappings, so nothing is mapped or unmapped here. Exit detaches, so
// pid becomes each segment's last operator, as on Linux.
func (t *shmTable) release(pid int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range t.attached[pid] {
		a.seg.nattch--
		a.seg.lpid = pid
		t.maybeFree(a.seg)
	}
	delete(t.attached, pid)
}

// close frees every segment, and later gets fail. The kernel calls it once
// no process runs.
func (t *shmTable) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, s := range t.segs {
		t.free(s)
	}
	clear(t.keys)
	clear(t.attached)
}

// stats counts live segments and attachments.
func (t *shmTable) stats() (segments, attachments int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.attached {
		attachments += len(l)
	}
	return len(t.segs), attachments
}

// locate finds the segment pid has mapped over [addr, addr+size), and the
// offset of addr in it.
func (t *shmTable) locate(pid int32, addr, size uint32) (seg int32, off uint32, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range t.attached[pid] {
		if addr >= a.addr && uint64(addr)+uint64(size) <= uint64(a.addr)+a.seg.obj.Size() {
			return a.seg.id, addr - a.addr, true
		}
	}
	return 0, 0, false
}

func (t *shmTable) maybeFree(s *shmSeg) {
	if s.removed && s.nattch == 0 {
		t.free(s)
	}
}

func (t *shmTable) free(s *shmSeg) {
	_ = s.obj.Close()
	delete(t.segs, s.id)
	if t.onFree != nil {
		t.onFree(s.id)
	}
}
