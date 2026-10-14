package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feverenergy/pgmem"
)

// Protocol codes of the untyped packets that get a one-byte answer.
const (
	sslRequestCode    = 80877103
	gssencRequestCode = 80877104

	maxStartupPacket = 10000 // Postgres's MAX_STARTUP_PACKET_LENGTH
	maxInspected     = 1 << 20
	dialTimeout      = 30 * time.Second
	closeWait        = 10 * time.Second // for sessions to end at close
)

// proxy accepts connections and copies each one's bytes to and from its
// own Server.Dial connection.
type proxy struct {
	srv *pgmem.Server
	sy  *outSyncer
	log *log.Logger

	ctx    context.Context // cancelled by close, ending dials in progress
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	lns    []net.Listener
	conns  map[net.Conn]struct{} // accepted, not yet closed
	wg     sync.WaitGroup        // accept loops and sessions
}

func newProxy(srv *pgmem.Server, sy *outSyncer, logger *log.Logger) *proxy {
	ctx, cancel := context.WithCancel(context.Background())
	return &proxy{srv: srv, sy: sy, log: logger, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
}

// serve accepts connections on ln until close.
func (p *proxy) serve(ln net.Listener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		ln.Close()
		return
	}
	p.log.Printf("listening on %s %s", ln.Addr().Network(), ln.Addr())
	p.lns = append(p.lns, ln)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.accept(ln)
	}()
}

func (p *proxy) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil { // such as EMFILE, so wait for a session to end
			p.log.Printf("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			c.Close()
			return
		}
		p.conns[c] = struct{}{}
		p.wg.Add(1) // the accept loop's own count keeps the counter above zero
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			p.session(c)
			p.mu.Lock()
			delete(p.conns, c)
			p.mu.Unlock()
		}()
	}
}

// close stops accepting and closes every client connection. That ends
// each session and so its server connection. close then waits for the
// sessions to end.
func (p *proxy) close() {
	p.mu.Lock()
	p.closed = true
	lns := p.lns
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, ln := range lns {
		ln.Close() // a Unix listener also removes its socket file
	}
	p.cancel()
	for _, c := range conns {
		c.Close()
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeWait):
		p.log.Printf("sessions still running %v after close", closeWait)
	}
}

// session proxies c until either side ends. Whichever direction ends
// first closes the server connection. The server-to-client copy closes
// both once it is done, so the other direction's blocked read or write
// ends too.
func (p *proxy) session(c net.Conn) {
	defer c.Close()
	ctx, cancel := context.WithTimeout(p.ctx, dialTimeout)
	s, err := p.srv.Dial(ctx, "unix", "")
	cancel()
	if err != nil {
		p.log.Printf("dial: %v", err)
		return
	}
	defer s.Close()
	single := make(chan struct{}, 4) // one-byte answers to SSL/GSS requests to expect
	var pending atomic.Bool          // a query named a synced directory, so sync at the next ReadyForQuery
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.serverToClient(s, c, single, &pending)
		c.Close()
		s.Close()
	}()
	p.clientToServer(c, s, single, done, &pending)
	s.Close()
	<-done
}

// clientToServer forwards the client's stream. It parses the untyped
// startup packets to tell serverToClient how many one-byte answers come
// before the typed messages. Those answer SSLRequest and GSSENCRequest.
// When directories are synced, it also parses the typed messages' framing.
// It closes single before it returns.
func (p *proxy) clientToServer(c, s net.Conn, single chan<- struct{}, peerDone <-chan struct{}, pending *atomic.Bool) {
	closeSingle := sync.OnceFunc(func() { close(single) })
	defer closeSingle()
	r := bufio.NewReaderSize(c, 64<<10)
	for {
		hdr, err := r.Peek(8)
		if err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		code := binary.BigEndian.Uint32(hdr[4:8])
		if n < 8 || n > maxStartupPacket {
			p.log.Printf("bad startup packet length %d", n)
			return
		}
		ssl := code == sslRequestCode || code == gssencRequestCode
		if ssl {
			select {
			case single <- struct{}{}:
			case <-peerDone:
				return
			}
		} else {
			closeSingle()
		}
		if _, err := io.CopyN(s, r, int64(n)); err != nil {
			return
		}
		if !ssl {
			break // the startup packet, or a CancelRequest
		}
	}
	if len(p.sy.dirs) == 0 {
		io.Copy(s, r)
		return
	}
	var msg []byte
	h := make([]byte, 5)
	for {
		if _, err := io.ReadFull(r, h); err != nil {
			return
		}
		n := int64(binary.BigEndian.Uint32(h[1:])) - 4
		if n < 0 {
			p.log.Printf("bad message length %d", n+4)
			return
		}
		if (h[0] == 'Q' || h[0] == 'P') && n <= maxInspected {
			// Before a query that names a synced directory reaches the
			// server, copy in files the client wrote there with psql's
			// \g FILE or \o FILE, so a server-side COPY FROM can read them.
			msg = slices.Grow(msg[:0], 5+int(n))[:5+int(n)]
			copy(msg, h)
			if _, err := io.ReadFull(r, msg[5:]); err != nil {
				return
			}
			if p.sy.mentions(msg[5:]) {
				p.sy.syncIn()
				pending.Store(true)
			}
			if _, err := s.Write(msg); err != nil {
				return
			}
			continue
		}
		if _, err := s.Write(h); err != nil {
			return
		}
		if _, err := io.CopyN(s, r, n); err != nil {
			return
		}
	}
}

// serverToClient forwards the server's stream. First come the one-byte
// answers clientToServer announces, then typed messages. When directories
// are synced, it copies them out to the host before it forwards the
// ReadyForQuery after a query that named one, such as lo_export or COPY TO
// a file.
func (p *proxy) serverToClient(s, c net.Conn, single <-chan struct{}, pending *atomic.Bool) {
	r := bufio.NewReaderSize(s, 64<<10)
	w := bufio.NewWriterSize(c, 64<<10)
	defer w.Flush()
	for range single {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		w.WriteByte(b)
		if err := w.Flush(); err != nil {
			return
		}
	}
	if len(p.sy.dirs) == 0 {
		io.Copy(c, r)
		return
	}
	hdr := make([]byte, 5)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return
		}
		n := int64(binary.BigEndian.Uint32(hdr[1:])) - 4
		if n < 0 {
			return
		}
		if hdr[0] == 'Z' && pending.Swap(false) {
			p.sy.syncOut()
		}
		if _, err := w.Write(hdr); err != nil {
			return
		}
		if _, err := io.CopyN(w, r, n); err != nil {
			return
		}
		if r.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}
