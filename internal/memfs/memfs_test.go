package memfs_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

func must(t *testing.T, err wasiabi.Errno) {
	t.Helper()
	if err != wasiabi.ESUCCESS {
		t.Fatalf("errno %d", err)
	}
}

func want(t *testing.T, got, want wasiabi.Errno) {
	t.Helper()
	if got != want {
		t.Fatalf("errno %d, want %d", got, want)
	}
}

func create(t *testing.T, fs *memfs.FS, path, data string) *memfs.Node {
	t.Helper()
	n, err := fs.Open(fs.Root(), path, memfs.OpenFlags{Create: true, Trunc: true})
	must(t, err)
	_, err = fs.WriteAt(n, []byte(data), 0)
	must(t, err)
	return n
}

func read(t *testing.T, fs *memfs.FS, n *memfs.Node) string {
	t.Helper()
	buf := make([]byte, fs.Stat(n).Size)
	got, err := fs.ReadAt(n, buf, 0)
	must(t, err)
	return string(buf[:got])
}

func TestNewHasDevices(t *testing.T) {
	fs := memfs.New()
	null, err := fs.Lookup(fs.Root(), "/dev/null", true)
	must(t, err)
	if fs.Stat(null).Kind != memfs.KindDevice {
		t.Fatal("/dev/null is not a device")
	}
	n, err := fs.WriteAt(null, []byte("abc"), 0)
	if err != 0 || n != 3 {
		t.Fatalf("write /dev/null: %d %d", n, err)
	}
	if n, _ := fs.ReadAt(null, make([]byte, 4), 0); n != 0 {
		t.Fatal("/dev/null read returned data")
	}
	r, err := fs.Lookup(fs.Root(), "/dev/urandom", true)
	must(t, err)
	b := make([]byte, 32)
	if n, err := fs.ReadAt(r, b, 0); err != 0 || n != 32 || bytes.Equal(b, make([]byte, 32)) {
		t.Fatal("/dev/urandom gave no random bytes")
	}
}

func TestCreateReadWriteTruncate(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "/a"))
	n := create(t, fs, "/a/f", "hello")
	if _, err := fs.WriteAt(n, []byte("XY"), 8); err != 0 {
		t.Fatal(err)
	}
	if got := read(t, fs, n); got != "hello\x00\x00\x00XY" {
		t.Fatalf("hole not zero-filled: %q", got)
	}
	must(t, fs.Truncate(n, 2))
	if got := read(t, fs, n); got != "he" {
		t.Fatalf("%q", got)
	}
	_, err := fs.Open(fs.Root(), "/a/f", memfs.OpenFlags{Create: true, Excl: true})
	want(t, err, wasiabi.EEXIST)
	_, err = fs.Open(fs.Root(), "/a/missing", memfs.OpenFlags{})
	want(t, err, wasiabi.ENOENT)
	_, err = fs.Open(fs.Root(), "/a/f", memfs.OpenFlags{Directory: true})
	want(t, err, wasiabi.ENOTDIR)
	_, err = fs.Open(fs.Root(), "/a/f/x", memfs.OpenFlags{Create: true})
	want(t, err, wasiabi.ENOTDIR)
	n2, err := fs.Open(fs.Root(), "/a/f", memfs.OpenFlags{Trunc: true})
	must(t, err)
	if fs.Stat(n2).Size != 0 {
		t.Fatal("O_TRUNC did not truncate")
	}
}

func TestRelativePathsDotsAndRoot(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "a"))
	must(t, fs.Mkdir(fs.Root(), "a/b"))
	b, err := fs.Lookup(fs.Root(), "a/b", true)
	must(t, err)
	create(t, fs, "a/x", "x")
	n, err := fs.Lookup(b, "../x", true)
	must(t, err)
	if read(t, fs, n) != "x" {
		t.Fatal("../x resolved wrongly")
	}
	if n, err := fs.Lookup(b, "/a/./b/..//x", true); err != 0 || read(t, fs, n) != "x" {
		t.Fatal("absolute path with . and .. resolved wrongly")
	}
	if n, err := fs.Lookup(fs.Root(), "..", true); err != 0 || n != fs.Root() {
		t.Fatal("/.. is not /")
	}
	_, err = fs.Lookup(fs.Root(), "a/x/", true)
	want(t, err, wasiabi.ENOTDIR)
	long := string(bytes.Repeat([]byte("n"), 256))
	_, err = fs.Open(fs.Root(), long, memfs.OpenFlags{Create: true})
	want(t, err, wasiabi.ENAMETOOLONG)
}

// pathOfLen returns a relative path of n bytes, "a/a/.../a" or one byte
// more, whose first component does not exist.
func pathOfLen(n int) string {
	p := strings.Repeat("a/", n/2)
	if n%2 == 1 {
		p += "a"
	}
	return p
}

// TestPathLimits checks that a path over MaxPathLen fails with
// ENAMETOOLONG before it is split, because splitting amplifies a path of
// slashes ~32x in memory. A symlink target over MaxSymlinkLen fails the
// same way. One at the limit resolves as usual.
func TestPathLimits(t *testing.T) {
	fs := memfs.New()
	if len(pathOfLen(memfs.MaxPathLen)) != memfs.MaxPathLen {
		t.Fatal("pathOfLen")
	}
	_, err := fs.Lookup(fs.Root(), pathOfLen(memfs.MaxPathLen), true)
	want(t, err, wasiabi.ENOENT)
	_, err = fs.Lookup(fs.Root(), pathOfLen(memfs.MaxPathLen+1), true)
	want(t, err, wasiabi.ENAMETOOLONG)
	want(t, fs.Mkdir(fs.Root(), strings.Repeat("/", memfs.MaxPathLen+1)), wasiabi.ENAMETOOLONG)
	if err := fs.WriteFile("/"+pathOfLen(memfs.MaxPathLen), nil); !errors.Is(err, wasiabi.ENAMETOOLONG) {
		t.Fatalf("WriteFile: %v", err)
	}

	want(t, fs.Symlink(strings.Repeat("t", memfs.MaxSymlinkLen+1), fs.Root(), "big"), wasiabi.ENAMETOOLONG)
	must(t, fs.Symlink(strings.Repeat("t", memfs.MaxSymlinkLen), fs.Root(), "max"))
	target, err := fs.Readlink(fs.Root(), "max")
	must(t, err)
	if len(target) != memfs.MaxSymlinkLen {
		t.Fatalf("target of %d bytes", len(target))
	}
}

func TestMkdirRmdirUnlink(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "/d"))
	want(t, fs.Mkdir(fs.Root(), "/d"), wasiabi.EEXIST)
	want(t, fs.Mkdir(fs.Root(), "/nope/d"), wasiabi.ENOENT)
	create(t, fs, "/d/f", "x")
	want(t, fs.Rmdir(fs.Root(), "/d"), wasiabi.ENOTEMPTY)
	want(t, fs.Rmdir(fs.Root(), "/d/f"), wasiabi.ENOTDIR)
	want(t, fs.Unlink(fs.Root(), "/d"), wasiabi.EISDIR)
	must(t, fs.Unlink(fs.Root(), "/d/f"))
	want(t, fs.Unlink(fs.Root(), "/d/f"), wasiabi.ENOENT)
	must(t, fs.Rmdir(fs.Root(), "/d"))
}

func TestRename(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "/d"))
	old := create(t, fs, "/d/old", "old")
	create(t, fs, "/d/new", "new")
	// A file renamed over an existing file replaces it.
	must(t, fs.Rename(fs.Root(), "/d/new", fs.Root(), "/d/old"))
	n, err := fs.Lookup(fs.Root(), "/d/old", true)
	must(t, err)
	if read(t, fs, n) != "new" {
		t.Fatal("rename did not replace")
	}
	if read(t, fs, old) != "old" {
		t.Fatal("an open node lost its data when its name was replaced")
	}
	_, err = fs.Lookup(fs.Root(), "/d/new", true)
	want(t, err, wasiabi.ENOENT)
	// Directories.
	must(t, fs.Mkdir(fs.Root(), "/e"))
	must(t, fs.Mkdir(fs.Root(), "/e/sub"))
	want(t, fs.Rename(fs.Root(), "/e", fs.Root(), "/e/sub/e2"), wasiabi.EINVAL)
	want(t, fs.Rename(fs.Root(), "/d/old", fs.Root(), "/e"), wasiabi.EISDIR)
	want(t, fs.Rename(fs.Root(), "/e", fs.Root(), "/d/old"), wasiabi.ENOTDIR)
	must(t, fs.Mkdir(fs.Root(), "/empty"))
	must(t, fs.Rename(fs.Root(), "/e", fs.Root(), "/empty"))
	if _, err := fs.Lookup(fs.Root(), "/empty/sub", true); err != 0 {
		t.Fatal("directory rename over an empty directory lost children")
	}
	must(t, fs.Mkdir(fs.Root(), "/full"))
	create(t, fs, "/full/x", "")
	must(t, fs.Mkdir(fs.Root(), "/src"))
	want(t, fs.Rename(fs.Root(), "/src", fs.Root(), "/full"), wasiabi.ENOTEMPTY)
	// Renaming a node onto itself is a no-op.
	must(t, fs.Rename(fs.Root(), "/d/old", fs.Root(), "/d/old"))
}

func TestLinksAndSymlinks(t *testing.T) {
	fs := memfs.New()
	create(t, fs, "/f", "data")
	must(t, fs.Link(fs.Root(), "/f", fs.Root(), "/h", false))
	f, _ := fs.Lookup(fs.Root(), "/f", true)
	if fs.Stat(f).Nlink != 2 {
		t.Fatal("nlink not 2")
	}
	must(t, fs.Unlink(fs.Root(), "/f"))
	h, err := fs.Lookup(fs.Root(), "/h", true)
	must(t, err)
	if fs.Stat(h).Nlink != 1 || read(t, fs, h) != "data" {
		t.Fatal("hard link lost data")
	}
	must(t, fs.Mkdir(fs.Root(), "/dir"))
	want(t, fs.Link(fs.Root(), "/dir", fs.Root(), "/dir2", false), wasiabi.EPERM)

	must(t, fs.Symlink("h", fs.Root(), "/s"))
	must(t, fs.Symlink("/dir", fs.Root(), "/sd"))
	must(t, fs.Symlink("nowhere", fs.Root(), "/dangling"))
	target, err := fs.Readlink(fs.Root(), "/s")
	must(t, err)
	if target != "h" {
		t.Fatalf("readlink %q", target)
	}
	_, err = fs.Readlink(fs.Root(), "/h")
	want(t, err, wasiabi.EINVAL)
	if n, err := fs.Lookup(fs.Root(), "/s", true); err != 0 || n != h {
		t.Fatal("symlink not followed")
	}
	if n, err := fs.Lookup(fs.Root(), "/s", false); err != 0 || fs.Stat(n).Kind != memfs.KindSymlink {
		t.Fatal("nofollow lookup did not return the link")
	}
	create(t, fs, "/sd/inner", "i")
	if _, err := fs.Lookup(fs.Root(), "/dir/inner", true); err != 0 {
		t.Fatal("path through a directory symlink failed")
	}
	_, err = fs.Lookup(fs.Root(), "/dangling", true)
	want(t, err, wasiabi.ENOENT)
	must(t, fs.Symlink("loop2", fs.Root(), "/loop1"))
	must(t, fs.Symlink("loop1", fs.Root(), "/loop2"))
	_, err = fs.Lookup(fs.Root(), "/loop1", true)
	want(t, err, wasiabi.ELOOP)
	want(t, fs.Symlink("x", fs.Root(), "/s"), wasiabi.EEXIST)
}

func TestReaddirSortedWithoutDots(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "/d"))
	for _, name := range []string{"b", "a", "c"} {
		create(t, fs, "/d/"+name, "")
	}
	must(t, fs.Mkdir(fs.Root(), "/d/z"))
	d, _ := fs.Lookup(fs.Root(), "/d", true)
	ents, err := fs.Readdir(d)
	must(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name)
	}
	if got := len(names); got != 4 || names[0] != "a" || names[3] != "z" || ents[3].Kind != memfs.KindDir {
		t.Fatalf("readdir %v", names)
	}
	f, _ := fs.Lookup(fs.Root(), "/d/a", true)
	_, err = fs.Readdir(f)
	want(t, err, wasiabi.ENOTDIR)
}

func TestTimesAndInodes(t *testing.T) {
	fs := memfs.New()
	n := create(t, fs, "/f", "x")
	m := create(t, fs, "/g", "y")
	if fs.Stat(n).Ino == 0 || fs.Stat(n).Ino == fs.Stat(m).Ino {
		t.Fatal("inode numbers not unique")
	}
	atim, mtim := int64(1_000), int64(2_000)
	fs.SetTimes(n, &atim, &mtim)
	if st := fs.Stat(n); st.Atim != 1_000 || st.Mtim != 2_000 {
		t.Fatalf("times %+v", st)
	}
	fs.SetTimes(n, nil, nil)
	if st := fs.Stat(n); st.Mtim != 2_000 {
		t.Fatal("nil times changed mtime")
	}
	before := fs.Stat(n).Mtim
	fs.WriteAt(n, []byte("z"), 0)
	if fs.Stat(n).Mtim <= before {
		t.Fatal("write did not advance mtime")
	}
}

func TestConvenienceHelpers(t *testing.T) {
	fs := memfs.New()
	if err := fs.WriteFile("/x/y/z.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("/x/y/z.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := fs.ReadFile("/nope"); err != wasiabi.ENOENT {
		t.Fatalf("ReadFile missing: %v", err)
	}
}

func TestConcurrentWriters(t *testing.T) {
	fs := memfs.New()
	must(t, fs.Mkdir(fs.Root(), "/c"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i))
			n, err := fs.Open(fs.Root(), "/c/"+name, memfs.OpenFlags{Create: true})
			if err != 0 {
				t.Error(err)
				return
			}
			for j := 0; j < 100; j++ {
				fs.WriteAt(n, []byte{byte(j)}, int64(j))
			}
		}(i)
	}
	wg.Wait()
	d, _ := fs.Lookup(fs.Root(), "/c", true)
	ents, _ := fs.Readdir(d)
	if len(ents) != 16 {
		t.Fatalf("%d entries", len(ents))
	}
}
