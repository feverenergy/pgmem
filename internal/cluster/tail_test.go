package cluster

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTailWriter(t *testing.T) {
	tw := &tailWriter{w: io.Discard, max: 20}
	for i := range 30 {
		fmt.Fprintf(tw, "line %d\n", i)
	}
	if got := strings.Split(tw.tail(), "\n"); len(got) != 21 || got[0] != "line 10" || got[19] != "line 29" {
		t.Fatalf("complete lines: %q", got)
	}

	// An unterminated line, written a byte at a time, keeps only its end
	// and counts among the 20.
	for i := range 3 * maxPartial {
		tw.Write([]byte{'a' + byte(i%26)})
	}
	if len(tw.partial) != maxPartial {
		t.Fatalf("partial line holds %d bytes, want %d", len(tw.partial), maxPartial)
	}
	got := strings.Split(tw.tail(), "\n")
	if len(got) != 21 || got[0] != "line 11" || got[19] != string(tw.partial) {
		t.Fatalf("with partial: first %q, %d lines", got[0], len(got))
	}

	// One large write is cut the same way, and a newline completes it.
	tw.Write([]byte(strings.Repeat("x", 3*maxPartial) + "y\nz"))
	got = strings.Split(tw.tail(), "\n")
	if last := got[len(got)-3]; len(last) != maxPartial || !strings.HasSuffix(last, "xy") {
		t.Fatalf("long line kept %d bytes ending %q", len(last), last[len(last)-2:])
	}
	if got[len(got)-2] != "z" {
		t.Fatalf("tail ends %q", got[len(got)-2])
	}
}
