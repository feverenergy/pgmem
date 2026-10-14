// Package pgwire is a minimal PostgreSQL frontend. It speaks just enough of
// protocol v3 to run one simple query on a trust-authenticated connection,
// so pgmem needs no client library.
package pgwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
)

// Exec runs one simple-protocol statement on conn and closes conn. It sends
// a protocol 3.0 startup message without SSL, waits for ReadyForQuery,
// sends sql as a Query, and reads until ReadyForQuery. The first
// ErrorResponse becomes the returned error, carrying its SQLSTATE and
// message. Exec handles only AuthenticationOk (trust auth). Any other
// authentication request is an error. A done ctx closes conn, which fails
// the exchange.
func Exec(ctx context.Context, conn net.Conn, user, database, sql string) (err error) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
	}()

	params := "user\x00" + user + "\x00database\x00" + database + "\x00\x00"
	start := binary.BigEndian.AppendUint32(nil, uint32(8+len(params)))
	start = binary.BigEndian.AppendUint32(start, 196608) // protocol 3.0
	if _, err := conn.Write(append(start, params...)); err != nil {
		return fmt.Errorf("pgwire: startup: %w", err)
	}
	if _, err := readUntilReady(conn); err != nil {
		return fmt.Errorf("pgwire: startup: %w", err)
	}
	if err := write(conn, 'Q', sql+"\x00"); err != nil {
		return fmt.Errorf("pgwire: query: %w", err)
	}
	qerr, err := readUntilReady(conn)
	if err != nil {
		return fmt.Errorf("pgwire: query: %w", err)
	}
	write(conn, 'X', "") // best effort, because the connection closes either way
	return qerr
}

// write sends one frontend message.
func write(w io.Writer, typ byte, body string) error {
	b := binary.BigEndian.AppendUint32([]byte{typ}, uint32(4+len(body)))
	_, err := w.Write(append(b, body...))
	return err
}

// readUntilReady reads backend messages through ReadyForQuery. It returns
// the first ErrorResponse as qerr. err is for protocol and I/O failures.
// The server may close the connection after an ErrorResponse, for a FATAL
// such as a startup failure. err then wraps that ErrorResponse, so its
// diagnostics are not lost.
func readUntilReady(r io.Reader) (qerr, err error) {
	for {
		var h [5]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if qerr != nil {
				return nil, fmt.Errorf("%w (then %v)", qerr, err)
			}
			return nil, err
		}
		n := binary.BigEndian.Uint32(h[1:])
		if n < 4 || n > 1<<24 {
			return nil, fmt.Errorf("bad message length %d", n)
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		switch h[0] {
		case 'Z':
			return qerr, nil
		case 'R':
			if len(body) < 4 || binary.BigEndian.Uint32(body) != 0 {
				return nil, fmt.Errorf("unsupported authentication request (only trust auth works)")
			}
		case 'E':
			if qerr == nil {
				qerr = parseError(body)
			}
		}
	}
}

// parseError turns an ErrorResponse body into an error with its SQLSTATE
// ('C') and message ('M').
func parseError(body []byte) error {
	var code, msg string
	for len(body) > 1 {
		field := body[0]
		val, rest, _ := strings.Cut(string(body[1:]), "\x00")
		body = []byte(rest)
		switch field {
		case 'C':
			code = val
		case 'M':
			msg = val
		}
	}
	return fmt.Errorf("postgres error %s: %s", code, msg)
}
