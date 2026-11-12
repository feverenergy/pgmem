package memfs_test

import (
	"archive/tar"
	"bytes"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
)

// File data lives in 64 KiB pages. These tests aim reads, writes and
// truncates at page boundaries and check the result against a plain slice.
const page = 64 << 10

// TestPagesMatchModel applies random writes and truncates that start or
// end near a page boundary, and after each one compares the whole file with
// a []byte that had the same operations applied.
func TestPagesMatchModel(t *testing.T) {
	fs := memfs.New()
	n := create(t, fs, "/f", "")
	var model []byte
	rng := rand.New(rand.NewPCG(1, 2))
	// near returns an offset within a few bytes of a page boundary, or a
	// small one, so the edge cases come up often.
	near := func() int {
		if rng.IntN(4) == 0 {
			return rng.IntN(64)
		}
		return rng.IntN(6)*page + rng.IntN(9) - 4
	}
	for i := 0; i < 400; i++ {
		switch rng.IntN(3) {
		case 0, 1: // write
			off := max(near(), 0)
			p := make([]byte, rng.IntN(2*page+3)+1)
			for j := range p {
				p[j] = byte(rng.IntN(255) + 1) // never zero, so zero fill shows
			}
			if _, err := fs.WriteAt(n, p, int64(off)); err != 0 {
				t.Fatalf("op %d: write %d bytes at %d: errno %d", i, len(p), off, err)
			}
			if end := off + len(p); end > len(model) {
				model = append(model, make([]byte, end-len(model))...)
			}
			copy(model[off:], p)
		case 2: // truncate, down or up
			size := max(near(), 0)
			must(t, fs.Truncate(n, int64(size)))
			if size < len(model) {
				model = model[:size]
			} else {
				model = append(model, make([]byte, size-len(model))...)
			}
		}
		if got := fs.Stat(n).Size; got != int64(len(model)) {
			t.Fatalf("op %d: size %d, want %d", i, got, len(model))
		}
		got := make([]byte, len(model)+10)
		k, err := fs.ReadAt(n, got, 0)
		if err != 0 || k != len(model) || !bytes.Equal(got[:k], model) {
			t.Fatalf("op %d: contents differ from the model (read %d bytes, errno %d, want %d bytes)", i, k, err, len(model))
		}
	}
}

// TestReadAcrossPageBoundary reads small windows that straddle each
// boundary of a three-page file.
func TestReadAcrossPageBoundary(t *testing.T) {
	fs := memfs.New()
	data := make([]byte, 3*page-5)
	for i := range data {
		data[i] = byte(i % 251)
	}
	n := create(t, fs, "/f", string(data))
	for _, b := range []int{page, 2 * page} {
		for _, off := range []int{b - 3, b - 1, b, b + 1} {
			got := make([]byte, 7)
			k, err := fs.ReadAt(n, got, int64(off))
			if err != 0 || k != 7 || !bytes.Equal(got, data[off:off+7]) {
				t.Fatalf("read 7 at %d: %q (n %d, errno %d), want %q", off, got[:k], k, err, data[off:off+7])
			}
		}
	}
	// A read that runs past the end stops there.
	got := make([]byte, 20)
	if k, _ := fs.ReadAt(n, got, int64(len(data)-8)); k != 8 || !bytes.Equal(got[:8], data[len(data)-8:]) {
		t.Fatalf("read past end: %d bytes", k)
	}
}

// TestLoadTarTruncatedHugeEntry loads a tar whose only entry claims 1 GiB
// but carries a few bytes. LoadTar must fail, and must not allocate the
// claimed size first.
func TestLoadTarTruncatedHugeEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Mode: 0o600, Size: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("only a little")); err != nil {
		t.Fatal(err)
	}
	// No Close: the entry is short and the archive is cut off there.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := memfs.New().LoadTar(bytes.NewReader(buf.Bytes()), "/")
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("LoadTar accepted a truncated entry")
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("LoadTar allocated %d MiB for a truncated entry", alloc>>20)
	}
}

// TestCtimNotBehindMtim links and unlinks a name for a file while another
// goroutine writes to it. Every change stamps ctim, so it must never fall
// behind mtim.
func TestCtimNotBehindMtim(t *testing.T) {
	fs := memfs.New()
	n := create(t, fs, "/f", "x")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fs.WriteAt(n, []byte("y"), 0)
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		must(t, fs.Link(fs.Root(), "/f", fs.Root(), "/g", false))
		must(t, fs.Unlink(fs.Root(), "/g"))
		if st := fs.Stat(n); st.Ctim < st.Mtim {
			close(stop)
			wg.Wait()
			t.Fatalf("iteration %d: ctim %d is behind mtim %d", i, st.Ctim, st.Mtim)
		}
	}
	close(stop)
	wg.Wait()
}
