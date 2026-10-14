package memfs_test

import (
	"archive/tar"
	"bytes"
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

func TestFileSizeCap(t *testing.T) {
	fs := memfs.New()
	n := create(t, fs, "/f", "x")
	if _, err := fs.WriteAt(n, []byte("x"), 1<<40); err != wasiabi.EFBIG {
		t.Fatalf("huge write offset: errno %d", err)
	}
	want(t, fs.Truncate(n, 1<<40), wasiabi.EFBIG)
	if size := fs.Stat(n).Size; size != 1 {
		t.Fatalf("size %d after EFBIG", size)
	}
	mtim := fs.Stat(n).Mtim
	if got, err := fs.WriteAt(n, nil, 100); err != 0 || got != 0 {
		t.Fatalf("zero-length write: %d %d", got, err)
	}
	if st := fs.Stat(n); st.Size != 1 || st.Mtim != mtim {
		t.Fatalf("zero-length write changed the file: %+v", st)
	}
}

func TestDevZero(t *testing.T) {
	fs := memfs.New()
	z, err := fs.Lookup(fs.Root(), "/dev/zero", true)
	must(t, err)
	b := []byte{1, 2, 3}
	if got, err := fs.ReadAt(z, b, 0); err != 0 || got != 3 || !bytes.Equal(b, []byte{0, 0, 0}) {
		t.Fatalf("/dev/zero read %d %d %v", got, err, b)
	}
}

func TestLinkChangesSetCtimNotAtim(t *testing.T) {
	fs := memfs.New()
	n := create(t, fs, "/f", "x")
	c0 := fs.Stat(n).Ctim
	must(t, fs.Link(fs.Root(), "/f", fs.Root(), "/g", false))
	c1 := fs.Stat(n).Ctim
	if c1 <= c0 {
		t.Fatal("link did not advance ctime")
	}
	must(t, fs.Rename(fs.Root(), "/g", fs.Root(), "/h"))
	if fs.Stat(n).Ctim <= c1 {
		t.Fatal("rename did not advance ctime")
	}
	a0 := fs.Stat(n).Atim
	fs.ReadAt(n, make([]byte, 1), 0)
	if fs.Stat(n).Atim != a0 {
		t.Fatal("read changed atime")
	}
}

func TestOpenNoFollow(t *testing.T) {
	fs := memfs.New()
	create(t, fs, "/t", "t")
	must(t, fs.Symlink("t", fs.Root(), "/s"))
	_, err := fs.Open(fs.Root(), "/s", memfs.OpenFlags{NoFollow: true})
	want(t, err, wasiabi.ELOOP)
	_, err = fs.Lookup(fs.Root(), "/s/", true)
	want(t, err, wasiabi.ENOTDIR)
}

func TestRemovedDirRefusesEntries(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "/d"))
	d, err := fs.Lookup(fs.Root(), "/d", true)
	must(t, err)
	must(t, fs.Rmdir(fs.Root(), "/d"))
	_, err = fs.Open(d, "x", memfs.OpenFlags{Create: true})
	want(t, err, wasiabi.ENOENT)
	want(t, fs.Mkdir(d, "y"), wasiabi.ENOENT)
	if p, err := fs.Lookup(d, "..", true); err != 0 || p != fs.Root() {
		t.Fatal("a removed directory lost its parent")
	}
}

func TestGrowAfterTruncateZeroFills(t *testing.T) {
	fs := memfs.New()
	n := create(t, fs, "/t", "abcdef")
	must(t, fs.Truncate(n, 2))
	if _, err := fs.WriteAt(n, []byte("Z"), 4); err != 0 {
		t.Fatal(err)
	}
	if got := read(t, fs, n); got != "ab\x00\x00Z" {
		t.Fatalf("stale bytes after truncate: %q", got)
	}
	must(t, fs.Truncate(n, 7))
	if got := read(t, fs, n); got != "ab\x00\x00Z\x00\x00" {
		t.Fatalf("truncate up: %q", got)
	}
}

func TestInodesNotReused(t *testing.T) {
	fs := memfs.New()
	old := fs.Stat(create(t, fs, "/t", "")).Ino
	must(t, fs.Unlink(fs.Root(), "/t"))
	if fs.Stat(create(t, fs, "/t", "")).Ino <= old {
		t.Fatal("inode number reused")
	}
}

func TestLoadTarContainsNamesAndRejectsOtherTypes(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	tw.Write([]byte("e"))
	tw.WriteHeader(&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo})
	tw.Close()

	fs := memfs.New()
	if err := fs.LoadTar(&buf, "/pg"); err == nil {
		t.Fatal("a FIFO entry was accepted")
	}
	if got, err := fs.ReadFile("/pg/escape"); err != nil || string(got) != "e" {
		t.Fatalf("\"..\" escaped the load directory: %q %v", got, err)
	}
	if _, err := fs.ReadFile("/escape"); err != wasiabi.ENOENT {
		t.Fatalf("/escape: %v", err)
	}
	if err := fs.WriteFile("/pg/escape/x", nil); err != wasiabi.ENOTDIR {
		t.Fatalf("WriteFile through a file: %v", err)
	}
}
