package pgmem

import (
	"io"
	"strings"
	"sync"
	"time"
)

// LogKeep is how much of the server log Server.Log keeps. It keeps the
// most recent LogKeep bytes.
const LogKeep = 256 << 10

// logQueue is how many chunks may wait for a slow Options.Log writer before
// new ones are dropped.
const logQueue = 256

// serverLog keeps the last LogKeep bytes of the server log and copies
// chunks to an optional writer without ever blocking the caller.
//
// Every guest process that logs writes here, through fds 1 and 2 shared
// with the postmaster, and the kernel serializes those writes. Postgres
// writes each line with one write(), so a chunk is a whole line. A Write
// that blocked would stall every process that logs.
type serverLog struct {
	mu     sync.Mutex
	ring   []byte        // the last LogKeep bytes at most
	out    chan []byte   // nil without Options.Log
	closed bool          // out is closed, so Write no longer sends
	stop   chan struct{} // closed by close, so the copier discards what is queued
	done   chan struct{} // closed when the copier has returned
}

// newServerLog returns a log that also copies to w, if w is not nil, from
// a goroutine that runs until close.
func newServerLog(w io.Writer) *serverLog {
	l := &serverLog{}
	if w != nil {
		l.out = make(chan []byte, logQueue)
		l.stop = make(chan struct{})
		l.done = make(chan struct{})
		go func() {
			defer close(l.done)
			for b := range l.out {
				select {
				case <-l.stop:
					return
				default:
				}
				_, _ = w.Write(b)
			}
		}()
	}
	return l
}

// Write keeps p in the ring and queues a copy for the writer, dropping it
// if the queue is full. It never blocks and never fails.
func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(p) >= LogKeep {
		l.ring = append(l.ring[:0], p[len(p)-LogKeep:]...)
	} else {
		if excess := len(l.ring) + len(p) - LogKeep; excess > 0 {
			l.ring = l.ring[excess:]
		}
		l.ring = append(l.ring, p...)
	}
	if l.out != nil && !l.closed {
		select {
		case l.out <- append([]byte(nil), p...):
		default: // the writer is behind, but the ring still has it
		}
	}
	return len(p), nil
}

// String returns the kept log.
func (l *serverLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.ring)
}

// tail returns the last n lines of the kept log.
func (l *serverLog) tail(n int) string {
	s := strings.TrimRight(l.String(), "\n")
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			if n--; n <= 0 {
				return s[i+1:]
			}
		}
	}
	return s
}

// close stops the copier. The copier discards the queued chunks and
// returns after the Write in progress, if any. close waits up to wait for
// that, then abandons the copier, so a writer that blocks forever cannot
// hang Close. Writes after close only reach the ring.
func (l *serverLog) close(wait time.Duration) {
	l.mu.Lock()
	if l.out == nil || l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	close(l.stop)
	close(l.out)
	l.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-l.done:
	case <-t.C:
	}
}
