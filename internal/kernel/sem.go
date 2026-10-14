package kernel

import (
	"math"
	"sync"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// POSIX semaphores live in the host. The key is where the guest's sem_t
// is. A sem_t in a shared segment is keyed by its offset there, so every
// process that attaches the segment sees the same semaphore at whatever
// address. Any other sem_t is keyed by its address in one process's
// private memory. wasi-libc's own semaphores cannot work across processes.
// Each process is a separate instance, and memory.atomic.wait in one never
// sees another's notify.

// semValueMax is SEM_VALUE_MAX.
const semValueMax = math.MaxInt32

// semSize is sizeof(sem_t) on wasm32.
const semSize = 16

// sem_wait modes.
const (
	semBlock = 0
	semTry   = 1
	semTimed = 2
)

type semKey struct {
	seg int32  // segment id, or 0 if private to pid
	pid int32  // 0 for a semaphore in a segment
	off uint32 // offset in the segment, or the address
}

type semaphore struct {
	value uint32
	// wake is closed and replaced whenever value rises or the semaphore
	// goes away, so waiters look again.
	wake chan struct{}
}

func (s *semaphore) wakeAll() {
	close(s.wake)
	s.wake = make(chan struct{})
}

type semTable struct {
	mu   sync.Mutex
	sems map[semKey]*semaphore
}

func newSemTable() *semTable { return &semTable{sems: map[semKey]*semaphore{}} }

// init creates the semaphore, or resets one that exists.
func (t *semTable) init(k semKey, value uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.sems[k]; s != nil {
		s.value = value
		s.wakeAll()
		return
	}
	t.sems[k] = &semaphore{value: value, wake: make(chan struct{})}
}

func (t *semTable) destroy(k semKey) wasiabi.Errno {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sems[k]
	if s == nil {
		return wasiabi.EINVAL
	}
	close(s.wake)
	delete(t.sems, k)
	return wasiabi.ESUCCESS
}

func (t *semTable) post(k semKey) wasiabi.Errno {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sems[k]
	if s == nil {
		return wasiabi.EINVAL
	}
	if s.value == semValueMax {
		return wasiabi.EOVERFLOW
	}
	s.value++
	s.wakeAll()
	return wasiabi.ESUCCESS
}

// take decrements the semaphore if it is positive, and then ok is true.
// Otherwise it returns the channel that fires when it is worth trying
// again.
func (t *semTable) take(k semKey) (ok bool, wake <-chan struct{}, errno wasiabi.Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sems[k]
	if s == nil {
		return false, nil, wasiabi.EINVAL
	}
	if s.value > 0 {
		s.value--
		return true, nil, wasiabi.ESUCCESS
	}
	return false, s.wake, wasiabi.ESUCCESS
}

func (t *semTable) value(k semKey) (uint32, wasiabi.Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sems[k]
	if s == nil {
		return 0, wasiabi.EINVAL
	}
	return s.value, wasiabi.ESUCCESS
}

// dropSegment removes the semaphores in a freed segment, waking waiters.
func (t *semTable) dropSegment(id int32) {
	t.drop(func(k semKey) bool { return k.seg == id })
}

// dropProcess removes an exiting process's private semaphores.
func (t *semTable) dropProcess(pid int32) {
	t.drop(func(k semKey) bool { return k.pid == pid })
}

func (t *semTable) drop(match func(semKey) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.sems {
		if match(k) {
			close(s.wake)
			delete(t.sems, k)
		}
	}
}

func (t *semTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sems)
}
