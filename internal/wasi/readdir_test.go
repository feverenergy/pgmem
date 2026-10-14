package wasi

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// readdir calls fd_readdir on fd 3 with a buffer of size bytes at 0 and
// returns the buffer and bufused.
func readdir(t *testing.T, p *Proc, size uint32, cookie uint64) ([]byte, uint32) {
	t.Helper()
	mem := &fakeMem{b: make([]byte, size+4)}
	c := call{mem: mem, p: p, stack: []uint64{3, 0, uint64(size), cookie, uint64(size)}}
	if err := fdReaddir(&c); err != wasiabi.ESUCCESS {
		t.Fatal(err)
	}
	return mem.b[:size], binary.LittleEndian.Uint32(mem.b[size:])
}

func TestReaddirFillsTheBufferWithAPartialEntry(t *testing.T) {
	fs := memfs.New()
	if err := fs.WriteFile("/d/name", nil); err != nil {
		t.Fatal(err)
	}
	d, _ := fs.Lookup(fs.Root(), "/d", true)
	p := &Proc{FS: fs, FDs: NewFDTable()}
	p.FDs.InstallAt(3, NewDesc(NewNodeFile(fs, d), 0))

	// "." is 25 bytes and ".." 26, so a 60-byte buffer ends inside "name".
	buf, used := readdir(t, p, 60, 0)
	if used != 60 {
		t.Fatalf("bufused %d, want a full buffer", used)
	}
	if next := binary.LittleEndian.Uint64(buf[25:]); next != 2 || string(buf[49:51]) != ".." {
		t.Fatalf("second entry: next %d name %q", next, buf[49:51])
	}
	// Resuming at cookie 2 returns the last entry whole, then the end.
	buf, used = readdir(t, p, 100, 2)
	if used != 28 || string(buf[24:28]) != "name" || binary.LittleEndian.Uint64(buf) != 3 {
		t.Fatalf("bufused %d entry %q", used, buf[:used])
	}
	if _, used = readdir(t, p, 100, 3); used != 0 {
		t.Fatalf("bufused %d past the end", used)
	}
}

// TestReaddirWhileUnlinking reads a directory the way wasi-libc does. It
// uses a 4 KiB buffer and resumes from the last whole entry's d_next. It
// unlinks each entry as it arrives, as Postgres's rmtree and
// RemovePgTempFilesInDir do. Every entry must come back exactly once.
func TestReaddirWhileUnlinking(t *testing.T) {
	const files = 400
	fs := memfs.New()
	for i := range files {
		if err := fs.WriteFile(fmt.Sprintf("/d/file%04d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := fs.Lookup(fs.Root(), "/d", true)
	p := &Proc{FS: fs, FDs: NewFDTable()}
	p.FDs.InstallAt(3, NewDesc(NewNodeFile(fs, d), 0))

	seen := map[string]int{}
	const size = 4096
	for cookie, more := uint64(0), true; more; {
		buf, used := readdir(t, p, size, cookie)
		for off := 0; off+wasiabi.SizeDirent <= int(used); {
			e := buf[off:]
			end := wasiabi.SizeDirent + int(binary.LittleEndian.Uint32(e[16:]))
			if off+end > int(used) {
				break // partial, so resume at cookie
			}
			name := string(e[wasiabi.SizeDirent:end])
			seen[name]++
			if name != "." && name != ".." {
				if err := fs.Unlink(d, name); err != wasiabi.ESUCCESS {
					t.Fatalf("unlink %s: %v", name, err)
				}
			}
			cookie = binary.LittleEndian.Uint64(e)
			off += end
		}
		more = used == size
	}
	for i := range files {
		if name := fmt.Sprintf("file%04d", i); seen[name] != 1 {
			t.Errorf("%s returned %d times", name, seen[name])
		}
	}
	if left, _ := fs.Readdir(d); len(left) != 0 {
		t.Fatalf("%d entries left behind", len(left))
	}
}
