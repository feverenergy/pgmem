package kernel

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/tetratelabs/wazero/api"
)

// Signal numbers the kernel itself acts on, in wasi-libc's numbering.
const (
	sigKILL = 9
	sigALRM = 14
	sigSTOP = 19

	sigCHLD  = 17
	sigCONT  = 18
	sigURG   = 23
	sigWINCH = 28

	maxSignal = 64 // masks are 64-bit, and bit sig-1 stands for sig
)

// unmaskable are the signals that can be neither blocked nor caught.
const unmaskable = 1<<(sigKILL-1) | 1<<(sigSTOP-1)

// defaultIgnored are the signals whose default action is to do nothing.
const defaultIgnored = 1<<(sigCHLD-1) | 1<<(sigCONT-1) | 1<<(sigURG-1) | 1<<(sigWINCH-1)

// dispatchExport is the compat layer's export that runs a process's
// deliverable signals. It is defined in postgres/compat/src/signal.c.
const dispatchExport = "pgmem_signal_dispatch"

// maxDispatchDepth caps nested dispatch. A handler's host call may
// dispatch again, but only this deep, so a stream of signals cannot grow
// the Go stack without bound.
const maxDispatchDepth = 4

func sigBit(sig int) uint64 { return 1 << (sig - 1) }

// sigBlock is the guest's struct pgmem_sigstate, in its memory. The guest
// and senders on other goroutines touch it only atomically.
type sigBlock struct {
	pending, blocked, ignored uint64
}

// signals is one process's signal state. It implements wasi.Signals for
// the process's blocking calls. It takes delivery from other processes, Go
// and the process's timer, and it dispatches after the process's host
// calls.
//
// Pending signals live in early until the compat layer registers the
// guest's sigBlock with pgm.sig_register from a constructor. After that
// they live in the sigBlock. detach drops the block under mu before the
// module's memory is freed, so a sender never writes to a dead process's
// memory. The module's close notifier and Kernel.exit both call detach.
type signals struct {
	p           *proc
	canDispatch bool // the module exports dispatchExport

	block atomic.Pointer[sigBlock] // stored under mu; nil if not registered or detached

	mu       sync.Mutex
	early    uint64        // signals sent before registration
	blocked  uint64        // the mask to start with, stored at registration
	detached bool          // the process is exiting, so drop everything
	wake     chan struct{} // closed and cleared by each sent signal; nil if nobody waits
	timer    *time.Timer   // the real-time timer; nil if disarmed
	timerGen uint64        // bumped by every setitimer, so a stale expiry does nothing
	interval time.Duration // the timer's re-arm interval

	// Used only on the process's goroutine, by afterCall.
	depth int
	fns   [maxDispatchDepth]api.Function // one per depth, so none on the stack is re-entered
}

func newSignals(p *proc, canDispatch bool, blocked uint64) *signals {
	return &signals{p: p, canDispatch: canDispatch, blocked: blocked &^ unmaskable}
}

// blockedMask returns the signals the process blocks now. Before the
// process registers, that is the mask it started with. Only the process's
// own goroutine calls it, while the module is alive.
func (s *signals) blockedMask() uint64 {
	if b := s.block.Load(); b != nil {
		return atomic.LoadUint64(&b.blocked)
	}
	return s.blocked
}

// Deliverable reports whether a pending signal is neither blocked nor
// ignored and could be dispatched now. At the dispatch depth cap it
// returns false. The signal then waits for the handlers on the stack to
// return. A blocking call in the deepest handler blocks, instead of
// failing with EINTR at once, again and again. Deliverable reads the
// guest's block and the depth, so only the process's own goroutine calls
// it, while the module is alive.
func (s *signals) Deliverable() bool {
	if s.depth >= maxDispatchDepth {
		return false
	}
	b := s.block.Load()
	if b == nil {
		return false
	}
	return atomic.LoadUint64(&b.pending)&^atomic.LoadUint64(&b.blocked)&^atomic.LoadUint64(&b.ignored) != 0
}

// Wake returns a channel closed by the next signal sent to the process.
func (s *signals) Wake() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	return s.wake
}

// wakeLocked wakes every waiter on Wake's channel. Each sent signal wakes
// them once. A signal that stays blocked does not keep them awake.
func (s *signals) wakeLocked() {
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
}

// send makes sig pending. sig is not SIGKILL. send discards a signal the
// process ignores at once. For a module without the compat layer's
// dispatch, the host carries out the default action. That is termination,
// unless the signal is ignored by default.
func (s *signals) send(sig int) {
	bit := sigBit(sig)
	s.mu.Lock()
	switch b := s.block.Load(); {
	case s.detached:
	case b != nil:
		if atomic.LoadUint64(&b.ignored)&bit == 0 {
			atomic.OrUint64(&b.pending, bit)
			s.wakeLocked()
		}
	case s.canDispatch:
		s.early |= bit
		s.wakeLocked()
	case defaultIgnored&bit == 0:
		s.mu.Unlock()
		s.p.terminate(sig)
		return
	}
	s.mu.Unlock()
}

// register attaches the guest's block. The mask the process started with
// becomes blocked first. Then signals sent earlier become pending, unless
// the guest ignores them. The host call's return dispatches those that are
// not blocked.
func (s *signals) register(b *sigBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached {
		return
	}
	atomic.OrUint64(&b.blocked, s.blocked)
	atomic.OrUint64(&b.pending, s.early&^atomic.LoadUint64(&b.ignored))
	s.early = 0
	s.block.Store(b)
}

// detach forgets the guest's block and stops the timer. Later signals are
// dropped. It runs before the module's memory is freed.
func (s *signals) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detached = true
	s.block.Store(nil)
	s.stopTimerLocked()
	s.wakeLocked()
}

func (s *signals) stopTimerLocked() {
	s.timerGen++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// setTimer arms the real-time timer to deliver SIGALRM after value and then
// every interval. An interval of 0 fires once. A zero value disarms the
// timer.
func (s *signals) setTimer(value, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopTimerLocked()
	if s.detached || value <= 0 {
		return
	}
	gen := s.timerGen
	s.interval = interval
	s.timer = time.AfterFunc(value, func() { s.expire(gen) })
}

// expire is the timer's callback. It re-arms an interval timer and sends
// SIGALRM, unless setitimer or exit has since replaced the timer.
func (s *signals) expire(gen uint64) {
	s.mu.Lock()
	if gen != s.timerGen {
		s.mu.Unlock()
		return
	}
	if s.interval > 0 {
		s.timer.Reset(s.interval)
	} else {
		s.timer = nil
	}
	s.mu.Unlock()
	s.send(sigALRM)
}

// usecDuration converts microseconds to a Duration, saturating.
func usecDuration(usec uint64) time.Duration {
	if usec > math.MaxInt64/uint64(time.Microsecond) {
		return math.MaxInt64
	}
	return time.Duration(usec) * time.Microsecond
}

// afterCall is the process's wasi.Proc.AfterCall. When a host call returns
// with a deliverable signal pending, it calls the guest's dispatch export
// before the guest resumes. A handler's own host calls may dispatch again,
// up to maxDispatchDepth. Each depth uses its own cached api.Function.
// Deeper signals wait for a later host call. If dispatch fails because of
// a trap, or because the process exited through signal_exit, afterCall
// panics again with the error. The guest never resumes, and the outer call
// ends with the error.
func (s *signals) afterCall(ctx context.Context, mod api.Module) {
	if !s.Deliverable() { // false at the depth cap
		return
	}
	f := s.fns[s.depth]
	if f == nil {
		if f = mod.ExportedFunction(dispatchExport); f == nil {
			return
		}
		s.fns[s.depth] = f
	}
	s.depth++
	_, err := f.Call(ctx)
	s.depth--
	if err != nil {
		panic(err)
	}
}

// signal delivers sig (1..maxSignal) to p. SIGKILL kills the process at its
// next host call, or as its current blocking call returns.
func (p *proc) signal(sig int) {
	if sig == sigKILL {
		p.terminate(sig)
		return
	}
	p.sigs.send(sig)
}

// terminate ends the process with "killed by sig". The first such signal
// wins, and the cancelled context ends the process at its next host
// call.
func (p *proc) terminate(sig int) {
	p.termSig.CompareAndSwap(0, int32(sig))
	p.cancel()
}

// sigBlockAt returns the block at addr in mem, or false if addr is not
// 8-byte aligned or the block is out of range. vmem never moves a memory,
// so the pointer stays valid until the module is closed.
func sigBlockAt(mem api.Memory, addr uint32) (*sigBlock, bool) {
	if addr%8 != 0 {
		return nil, false
	}
	b, ok := mem.Read(addr, uint32(unsafe.Sizeof(sigBlock{})))
	if !ok {
		return nil, false
	}
	return (*sigBlock)(unsafe.Pointer(&b[0])), true
}
