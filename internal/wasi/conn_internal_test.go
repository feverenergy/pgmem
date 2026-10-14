package wasi

import (
	"errors"
	"net"
	"testing"
	"time"
)

// Setting a deadline on a closed conn fails with net.ErrClosed and arms no
// timer.
func TestConnDeadlineAfterClose(t *testing.T) {
	a, b := NewSockPair()
	defer b.Close()
	c := NewConn(a).(*conn)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	for name, set := range map[string]func(time.Time) error{
		"SetDeadline":      c.SetDeadline,
		"SetReadDeadline":  c.SetReadDeadline,
		"SetWriteDeadline": c.SetWriteDeadline,
	} {
		if err := set(future); !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s after Close: %v, want net.ErrClosed", name, err)
		}
	}
	if c.rd.timer != nil || c.wd.timer != nil {
		t.Fatal("a deadline set after Close armed a timer")
	}
}
