package wasi

import (
	"sync"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// PipeCapacity is the buffer size of a pipe.
const PipeCapacity = 64 << 10

// pipeBuf is wasi-libc's PIPE_BUF. A write of at most this many bytes is
// atomic. It is never interleaved with other writers' bytes or cut short.
const pipeBuf = 4096

// NewPipe returns the read and write ends of a bounded pipe. Neither end
// ever blocks, whatever the flags. A read with nothing buffered and the
// writer open fails with EAGAIN, and so does a write with no room. Reads
// see EOF once the writer closed and the buffer drained. Writes fail with
// EPIPE once the reader closed. Every state change broadcasts the pipe's
// wait queue, which both ends return from Changed. A blocking fd_read or
// fd_write, and poll_oneoff, wait on it before trying again.
//
// A closed end fails every call with EBADF, so an fd_close on one thread
// wakes a call waiting on the same end on another, which then sees EBADF.
func NewPipe() (r, w File) {
	p := newPipe(NewNotifier(), PipeCapacity)
	return &pipeReader{pipeEnd{p: p}}, &pipeWriter{pipeEnd{p: p}}
}

// newPipe returns a pipe of the given capacity that broadcasts q at every
// state change. Sockets build their two directions from it.
func newPipe(q *Notifier, capacity int) *pipe { return &pipe{q: q, cap: capacity} }

// pipe is the state both ends share.
type pipe struct {
	q   *Notifier // broadcast at every state change
	cap int       // buffer size
	mu  sync.Mutex

	buf        []byte // ring of cap bytes, made at the first write
	head, size int    // buffered bytes are buf[head:head+size], wrapping

	rclosed, wclosed bool
}

// changed wakes every call waiting for the pipe to change: blocking
// fd_read and fd_write, and poll_oneoff. The caller holds mu, so a waiter
// that took the queue's channel before trying sees either the new state or
// the broadcast.
func (p *pipe) changed() { p.q.Broadcast() }

// take moves up to len(b) buffered bytes into b.
func (p *pipe) take(b []byte) int {
	n := 0
	for n < len(b) && p.size > 0 {
		k := min(len(b)-n, p.size, len(p.buf)-p.head)
		copy(b[n:], p.buf[p.head:p.head+k])
		p.head = (p.head + k) % len(p.buf)
		p.size -= k
		n += k
	}
	if p.size == 0 {
		p.head = 0
	}
	return n
}

// peekBytes copies up to len(b) buffered bytes into b without consuming
// them.
func (p *pipe) peekBytes(b []byte) int {
	n := 0
	for n < len(b) && n < p.size {
		at := (p.head + n) % len(p.buf)
		k := min(len(b)-n, p.size-n, len(p.buf)-at)
		copy(b[n:], p.buf[at:at+k])
		n += k
	}
	return n
}

// put buffers as much of b as fits.
func (p *pipe) put(b []byte) int {
	if p.buf == nil {
		p.buf = make([]byte, p.cap)
	}
	n := 0
	for n < len(b) && p.size < len(p.buf) {
		tail := (p.head + p.size) % len(p.buf)
		k := min(len(b)-n, len(p.buf)-p.size, len(p.buf)-tail)
		copy(p.buf[tail:], b[n:n+k])
		p.size += k
		n += k
	}
	return n
}

// pipeEnd is what both ends share. A pipe end has no offset and no type,
// and it is never seekable.
type pipeEnd struct {
	BaseFile
	p *pipe
}

// Filetype is unknown, because preview1 has no FIFO type.
func (pipeEnd) Filetype() uint8 { return wasiabi.FiletypeUnknown }

// Changed returns the pipe's wait channel, for Waitable.
func (e pipeEnd) Changed() <-chan struct{} { return e.p.q.Changed() }

// read never blocks. With nothing buffered and the writer open, it fails
// with EAGAIN. With peek the bytes stay buffered.
func (p *pipe) read(b []byte, peek bool) (int, wasiabi.Errno) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.rclosed:
		return 0, wasiabi.EBADF
	case len(b) == 0:
		return 0, wasiabi.ESUCCESS
	case p.size > 0:
		if peek {
			return p.peekBytes(b), wasiabi.ESUCCESS
		}
		n := p.take(b)
		p.changed()
		return n, wasiabi.ESUCCESS
	case p.wclosed:
		return 0, wasiabi.ESUCCESS // EOF
	}
	return 0, wasiabi.EAGAIN
}

// readReady reports the read end readable when data is buffered or the
// writer closed, which is a hang-up. A closed read end reports ready, so a
// poll racing fd_close returns and the next call sees EBADF.
func (p *pipe) readReady() (bool, uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rclosed {
		return true, 0, false
	}
	return p.size > 0 || p.wclosed, uint64(p.size), p.wclosed
}

// closeRead is idempotent. It drops buffered data, and writers get EPIPE.
func (p *pipe) closeRead() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.rclosed {
		p.rclosed = true
		p.buf, p.head, p.size = nil, 0, 0
		p.changed()
	}
}

// write never blocks. It buffers what fits and returns the count, or fails
// with EAGAIN when nothing fits. Up to pipeBuf bytes go in at once or not
// at all. A larger write goes in as far as there is room.
func (p *pipe) write(b []byte) (int, wasiabi.Errno) {
	p.mu.Lock()
	defer p.mu.Unlock()
	need := len(b)
	if need > pipeBuf {
		need = 1
	}
	switch {
	case len(b) == 0:
		return 0, wasiabi.ESUCCESS
	case p.wclosed:
		return 0, wasiabi.EBADF
	case p.rclosed:
		return 0, wasiabi.EPIPE
	case p.cap-p.size < need:
		return 0, wasiabi.EAGAIN
	}
	n := p.put(b)
	p.changed()
	return n, wasiabi.ESUCCESS
}

// writeReady reports the write end writable when a PIPE_BUF write fits, so
// a non-blocking write after POLLOUT never gets EAGAIN. It also reports it
// writable when the reader closed. That is a hang-up, and the write fails
// with EPIPE. nbytes is the free room. A closed write end reports ready.
func (p *pipe) writeReady() (bool, uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.wclosed:
		return true, 0, false
	case p.rclosed:
		return true, 0, true
	}
	room := p.cap - p.size
	return room >= pipeBuf, uint64(room), false
}

// closeWrite is idempotent. Readers see EOF once the buffer drains.
func (p *pipe) closeWrite() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.wclosed {
		p.wclosed = true
		if p.rclosed {
			p.buf = nil
		}
		p.changed()
	}
}

// pipeReader is the read end.
type pipeReader struct{ pipeEnd }

// Read never blocks. With nothing buffered and the writer open, it fails
// with EAGAIN, whatever the flags.
func (r *pipeReader) Read(b []byte, _ uint16) (int, wasiabi.Errno) { return r.p.read(b, false) }

// Ready reports poll readiness. The read end is never writable, but a
// closed end reports ready for any event.
func (r *pipeReader) Ready(event uint8) (bool, uint64, bool) {
	if event != wasiabi.EventtypeFdRead {
		r.p.mu.Lock()
		defer r.p.mu.Unlock()
		return r.p.rclosed, 0, false
	}
	return r.p.readReady()
}

// Close is idempotent.
func (r *pipeReader) Close() wasiabi.Errno { r.p.closeRead(); return wasiabi.ESUCCESS }

// pipeWriter is the write end.
type pipeWriter struct{ pipeEnd }

// Write never blocks. It buffers what fits and returns the count, or fails
// with EAGAIN when nothing fits, whatever the flags.
func (w *pipeWriter) Write(b []byte, _ uint16) (int, wasiabi.Errno) { return w.p.write(b) }

// Ready reports poll readiness. The write end is never readable, but a
// closed end reports ready for any event.
func (w *pipeWriter) Ready(event uint8) (bool, uint64, bool) {
	if event != wasiabi.EventtypeFdWrite {
		w.p.mu.Lock()
		defer w.p.mu.Unlock()
		return w.p.wclosed, 0, false
	}
	return w.p.writeReady()
}

// Close is idempotent.
func (w *pipeWriter) Close() wasiabi.Errno { w.p.closeWrite(); return wasiabi.ESUCCESS }
