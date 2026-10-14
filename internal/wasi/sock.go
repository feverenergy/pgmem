package wasi

import (
	"sync"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// SockCapacity is the buffer size of each direction of a stream socket.
const SockCapacity = 256 << 10

// maxBacklog bounds a listener's queue.
const maxBacklog = 4096

type sockState uint8

const (
	sockUnbound sockState = iota // fresh, possibly bound
	sockListening
	sockConnected
	sockClosed
)

// Sock is a Unix-domain stream socket. It is fresh and possibly bound to
// a path, or listening, or connected. A connected socket is one end of a
// pair whose two directions are pipes, and the pair shares one wait queue.
// As with pipes, no call ever blocks, and callers wait on Changed. A
// socket's state only moves forward: unbound, then listening or connected,
// then closed.
//
// Lock order: Connect holds the client's mu, then the listener's. A
// listener never locks a client. A socket holds its mu around calls into
// its pipes, and the pipes' locks never take a socket's. onClose runs
// outside mu.
type Sock struct {
	BaseFile

	mu    sync.Mutex
	state sockState
	q     *Notifier // the current state's queue, shared with the peer when connected
	bound bool
	path  string // bound address, or the listener's for an accepted socket
	peer  string // the connected peer's address, or "" when unnamed

	onClose func() // from Bind, and runs once at Close

	// listening
	backlog int
	queue   []*Sock

	// connected
	rx, tx *pipe // rx is what this end reads, tx what the peer reads

	// Local shutdowns. The pipe ends they close answer EBADF, so Read and
	// Write answer EOF and EPIPE from these flags before reaching them.
	shutRd, shutWr bool
}

// NewSock returns a fresh, unbound socket.
func NewSock() *Sock { return &Sock{q: NewNotifier()} }

// NewSockPair returns two connected, unnamed sockets.
func NewSockPair() (a, b *Sock) {
	q := NewNotifier()
	ab, ba := newPipe(q, SockCapacity), newPipe(q, SockCapacity)
	a = &Sock{state: sockConnected, q: q, rx: ba, tx: ab}
	b = &Sock{state: sockConnected, q: q, rx: ab, tx: ba}
	return a, b
}

// setQueue moves the socket to a new state's queue and wakes the waiters
// on the old one, so they look again. The caller holds mu.
func (s *Sock) setQueue(q *Notifier) {
	old := s.q
	s.q = q
	old.Broadcast()
}

// Filetype is a stream socket.
func (*Sock) Filetype() uint8 { return wasiabi.FiletypeSocketStream }

// Changed returns the wait channel of the socket's current state, for
// Waitable. A state change closes it too.
func (s *Sock) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.q.Changed()
}

// Bind records the socket's address. onClose, if not nil, runs once when
// the socket closes. Bind fails with EINVAL if the socket is already bound
// or connected.
func (s *Sock) Bind(path string, onClose func()) wasiabi.Errno {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errno := s.canBind(); errno != wasiabi.ESUCCESS {
		return errno
	}
	s.bound, s.path, s.onClose = true, path, onClose
	return wasiabi.ESUCCESS
}

// CanBind returns what Bind would fail with now. That is EBADF once the
// socket is closed, EINVAL once it is bound or connected, and ESUCCESS
// otherwise.
func (s *Sock) CanBind() wasiabi.Errno {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canBind()
}

func (s *Sock) canBind() wasiabi.Errno {
	switch {
	case s.state == sockClosed:
		return wasiabi.EBADF
	case s.bound || s.state != sockUnbound:
		return wasiabi.EINVAL
	}
	return wasiabi.ESUCCESS
}

// Listen makes a bound socket accept connections, with at most backlog of
// them queued. backlog is clamped to [1, 4096]. Calling Listen again
// changes the backlog.
func (s *Sock) Listen(backlog int) wasiabi.Errno {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == sockClosed:
		return wasiabi.EBADF
	case !s.bound || s.state == sockConnected:
		return wasiabi.EINVAL
	}
	s.backlog = min(max(backlog, 1), maxBacklog)
	if s.state != sockListening {
		s.state = sockListening
		s.setQueue(NewNotifier())
	}
	return wasiabi.ESUCCESS
}

// Listening reports whether the socket accepts connections.
func (s *Sock) Listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == sockListening
}

// Connect connects the socket to the listener l. The server end goes on
// l's queue for Accept, and the connection carries data at once.
//
// Connect checks l first, under l's lock alone, so a socket that is not
// listening refuses before s is locked. Otherwise two bound sockets
// connecting to each other at once would each hold its own lock and wait
// for the other's. Once l listens it can only close. Connect on a
// listening or closed socket returns before locking another. So holding
// s.mu while waiting for l.mu cannot close a cycle.
func (s *Sock) Connect(l *Sock) wasiabi.Errno {
	if !l.Listening() {
		return wasiabi.ECONNREFUSED
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case sockClosed:
		return wasiabi.EBADF
	case sockConnected:
		return wasiabi.EISCONN
	case sockListening:
		return wasiabi.EINVAL
	}
	if l == s {
		return wasiabi.ECONNREFUSED // s is not listening
	}
	l.mu.Lock() // check again, because l may have closed since
	defer l.mu.Unlock()
	switch {
	case l.state != sockListening:
		return wasiabi.ECONNREFUSED
	case len(l.queue) >= l.backlog:
		return wasiabi.EAGAIN
	}
	q := NewNotifier()
	cs, sc := newPipe(q, SockCapacity), newPipe(q, SockCapacity)
	// Nobody else sees the server end until it is queued, so it needs no
	// lock.
	srv := &Sock{state: sockConnected, q: q, bound: true, path: l.path, peer: s.path, rx: cs, tx: sc}
	s.state, s.rx, s.tx, s.peer = sockConnected, sc, cs, l.path
	s.setQueue(q)
	l.queue = append(l.queue, srv)
	l.q.Broadcast()
	return wasiabi.ESUCCESS
}

// Accept pops the oldest queued connection. It fails with EAGAIN while
// there is none.
func (s *Sock) Accept() (*Sock, wasiabi.Errno) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == sockClosed:
		return nil, wasiabi.EBADF
	case s.state != sockListening:
		return nil, wasiabi.EINVAL
	case len(s.queue) == 0:
		return nil, wasiabi.EAGAIN
	}
	c := s.queue[0]
	s.queue[0] = nil
	s.queue = s.queue[1:]
	s.q.Broadcast() // room for a connect that found the queue full
	return c, wasiabi.ESUCCESS
}

// Addr returns the bound address. For an accepted socket, that is the
// listener's.
func (s *Sock) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// PeerAddr returns the connected peer's address, or "" when it is
// unnamed.
func (s *Sock) PeerAddr() (string, wasiabi.Errno) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case sockConnected:
		return s.peer, wasiabi.ESUCCESS
	case sockClosed:
		return "", wasiabi.EBADF
	}
	return "", wasiabi.ENOTCONN
}

// Read never blocks. With nothing buffered and the peer still able to
// write, it fails with EAGAIN. It reads EOF after the peer shut its write
// side or closed, or after a local SHUT_RD. An unconnected socket,
// listening or not, gives ENOTCONN.
func (s *Sock) Read(b []byte, _ uint16) (int, wasiabi.Errno) { return s.recv(b, false) }

// peek reads like Read but leaves the bytes buffered.
func (s *Sock) peek(b []byte) (int, wasiabi.Errno) { return s.recv(b, true) }

func (s *Sock) recv(b []byte, peek bool) (int, wasiabi.Errno) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case sockClosed:
		return 0, wasiabi.EBADF
	case sockConnected:
		if s.shutRd {
			return 0, wasiabi.ESUCCESS // EOF
		}
		return s.rx.read(b, peek)
	}
	return 0, wasiabi.ENOTCONN
}

// Write never blocks. It buffers what fits, or fails with EAGAIN when the
// buffer is full. It fails with EPIPE, and sends no signal, after the peer
// shut its read side or closed, or after a local SHUT_WR. An unconnected
// socket, listening or not, gives ENOTCONN.
func (s *Sock) Write(b []byte, _ uint16) (int, wasiabi.Errno) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case sockClosed:
		return 0, wasiabi.EBADF
	case sockConnected:
		if s.shutWr {
			return 0, wasiabi.EPIPE
		}
		return s.tx.write(b)
	}
	return 0, wasiabi.ENOTCONN
}

// Ready reports poll readiness. A listening socket is readable while a
// connection is queued. A connected one follows its pipes. After a local
// shutdown, the pipe end it closed reports ready, because Read (EOF) and
// Write (EPIPE) then return at once. A closed socket reports ready for any
// event, so a racing poll returns and the next call sees EBADF.
func (s *Sock) Ready(event uint8) (bool, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case sockClosed:
		return true, 0, false
	case sockListening:
		if event == wasiabi.EventtypeFdRead && len(s.queue) > 0 {
			return true, uint64(len(s.queue)), false
		}
	case sockConnected:
		switch event {
		case wasiabi.EventtypeFdRead:
			return s.rx.readReady()
		case wasiabi.EventtypeFdWrite:
			return s.tx.writeReady()
		}
	}
	return false, 0, false
}

// Shutdown shuts the read side (SdflagsRd), the write side (SdflagsWr) or
// both. The peer reads EOF once this side's writes are shut, and its
// writes fail with EPIPE once this side's reads are.
func (s *Sock) Shutdown(how uint8) wasiabi.Errno {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == sockClosed:
		return wasiabi.EBADF
	case how == 0 || how&^(wasiabi.SdflagsRd|wasiabi.SdflagsWr) != 0:
		return wasiabi.EINVAL
	case s.state != sockConnected:
		return wasiabi.ENOTCONN
	}
	if how&wasiabi.SdflagsRd != 0 {
		s.shutRd = true
		s.rx.closeRead() // broadcasts the pair's queue
	}
	if how&wasiabi.SdflagsWr != 0 {
		s.shutWr = true
		s.tx.closeWrite()
	}
	return wasiabi.ESUCCESS
}

// Close is idempotent. A connected socket shuts both sides, so its peer
// reads EOF and its writes fail with EPIPE. A listener closes the
// connections it never handed out. Every waiter on the socket, and on its
// peer, wakes. onClose runs last, outside the lock.
func (s *Sock) Close() wasiabi.Errno {
	s.mu.Lock()
	if s.state == sockClosed {
		s.mu.Unlock()
		return wasiabi.ESUCCESS
	}
	var queued []*Sock
	switch s.state {
	case sockConnected:
		s.shutRd, s.shutWr = true, true
		s.rx.closeRead()
		s.tx.closeWrite()
	case sockListening:
		queued, s.queue = s.queue, nil
	}
	s.state = sockClosed
	q, onClose := s.q, s.onClose
	s.onClose = nil
	s.mu.Unlock()

	// No connection can join the queue any more, because Connect sees
	// sockClosed.
	for _, c := range queued {
		c.Close()
	}
	q.Broadcast()
	if onClose != nil {
		onClose()
	}
	return wasiabi.ESUCCESS
}
