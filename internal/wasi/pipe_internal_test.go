package wasi

import (
	"testing"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

func TestPipePeek(t *testing.T) {
	p := newPipe(NewNotifier(), 8)
	p.write([]byte("abcdef"))
	p.read(make([]byte, 5), false) // head moves, then the data wraps
	p.write([]byte("ghijk"))
	b := make([]byte, 16)
	n, err := p.read(b, true)
	if err != wasiabi.ESUCCESS || string(b[:n]) != "fghijk" {
		t.Fatalf("peek %q %d", b[:n], err)
	}
	n, _ = p.read(b, false)
	if string(b[:n]) != "fghijk" {
		t.Fatalf("read after peek %q", b[:n])
	}
}
