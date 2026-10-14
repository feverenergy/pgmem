package pgmem

import (
	"strings"
	"testing"
	"time"
)

func TestServerLogRingAndTail(t *testing.T) {
	l := newServerLog(nil)
	if got := l.tail(30); got != "" {
		t.Fatalf("tail of an empty log: %q", got)
	}
	line := strings.Repeat("x", 99) + "\n"
	for range 2 * LogKeep / len(line) {
		l.Write([]byte(line))
	}
	l.Write([]byte("a\nb\nc\n"))
	if s := l.String(); len(s) != LogKeep || !strings.HasSuffix(s, "a\nb\nc\n") {
		t.Fatalf("ring holds %d bytes, ending %q", len(s), s[len(s)-10:])
	}
	if got := l.tail(2); got != "b\nc" {
		t.Fatalf("tail(2) = %q", got)
	}
	big := strings.Repeat("y", LogKeep+10)
	l.Write([]byte(big))
	if l.String() != big[10:] {
		t.Fatal("a write larger than LogKeep does not keep its end")
	}
}

// A writer that blocks makes the log drop chunks, never block, and close
// gives up on it after its bound.
func TestServerLogStalledWriter(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	l := newServerLog(writerFunc(func(p []byte) (int, error) { <-block; return len(p), nil }))
	t0 := time.Now()
	const n = 2000
	for range n {
		l.Write([]byte("0123456789\n"))
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("%d writes to a stalled log took %v", n, d)
	}
	if got := len(l.String()); got != 11*n {
		t.Fatalf("ring holds %d bytes, want %d", got, 11*n)
	}
	t0 = time.Now()
	l.close(100 * time.Millisecond)
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("close took %v", d)
	}
	l.Write([]byte("after close\n")) // only the ring; must not panic
	if !strings.HasSuffix(l.String(), "after close\n") {
		t.Fatal("a write after close missed the ring")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
