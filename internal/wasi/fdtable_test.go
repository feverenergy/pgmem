package wasi_test

import (
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

type countingFile struct {
	wasi.BaseFile
	closes int
}

func (f *countingFile) Close() wasiabi.Errno { f.closes++; return 0 }

// install installs d in tab and fails the test if it cannot.
func install(t *testing.T, tab *wasi.FDTable, d *wasi.Desc) int32 {
	t.Helper()
	fd, err := tab.Install(d)
	if err != wasiabi.ESUCCESS {
		t.Fatalf("Install: %d", err)
	}
	return fd
}

func TestInstallTakesLowestFree(t *testing.T) {
	tab := wasi.NewFDTable()
	a := install(t, tab, wasi.NewDesc(&countingFile{}, 0))
	b := install(t, tab, wasi.NewDesc(&countingFile{}, 0))
	if a != 0 || b != 1 {
		t.Fatalf("%d %d", a, b)
	}
	tab.Close(0)
	if c := install(t, tab, wasi.NewDesc(&countingFile{}, 0)); c != 0 {
		t.Fatalf("reused %d", c)
	}
}

func TestDescriptionClosesWithLastReference(t *testing.T) {
	f := &countingFile{}
	parent, child := wasi.NewFDTable(), wasi.NewFDTable()
	fd := install(t, parent, wasi.NewDesc(f, 0))
	d, err := parent.Dup(fd)
	if err != 0 {
		t.Fatal(err)
	}
	child.InstallAt(4, d)
	parent.Close(fd)
	if f.closes != 0 {
		t.Fatal("closed while the child still holds it")
	}
	child.CloseAll()
	if f.closes != 1 {
		t.Fatalf("closes %d", f.closes)
	}
	if _, err := child.Get(4); err != wasiabi.EBADF {
		t.Fatal("fd still open after CloseAll")
	}
}

func TestRenumber(t *testing.T) {
	tab := wasi.NewFDTable()
	f1, f2 := &countingFile{}, &countingFile{}
	tab.InstallAt(5, wasi.NewDesc(f1, 0))
	tab.InstallAt(7, wasi.NewDesc(f2, 0))
	if err := tab.Renumber(5, 7); err != 0 {
		t.Fatal(err)
	}
	if f2.closes != 1 {
		t.Fatal("target not closed")
	}
	if _, err := tab.Get(5); err != wasiabi.EBADF {
		t.Fatal("source still open")
	}
	if d, _ := tab.Get(7); d.File() != f1 {
		t.Fatal("wrong file at target")
	}
	if err := tab.Renumber(9, 7); err != wasiabi.EBADF {
		t.Fatal("renumber from a closed fd")
	}
}

func TestRootTableLayout(t *testing.T) {
	tab := wasi.NewRootFDs(memfs.New(), nil, nil, nil)
	if tab.Len() != 4 {
		t.Fatalf("len %d", tab.Len())
	}
	d, err := tab.Get(3)
	if err != 0 || d.Preopen() != "/" || d.File().DirNode() == nil {
		t.Fatal("fd 3 is not the root preopen")
	}
	child := wasi.NewChildFDs(memfs.New())
	if d, err := child.Get(3); err != 0 || d.Preopen() != "/" {
		t.Fatal("child fd 3 is not the root preopen")
	}
}

func TestCloexecRoundTrip(t *testing.T) {
	tab := wasi.NewFDTable()
	fd := install(t, tab, wasi.NewDesc(&countingFile{}, 0))
	if on, err := tab.Cloexec(fd); err != 0 || on {
		t.Fatalf("new fd: cloexec %v err %d", on, err)
	}
	if err := tab.SetCloexec(fd, true); err != 0 {
		t.Fatal(err)
	}
	if on, err := tab.Cloexec(fd); err != 0 || !on {
		t.Fatalf("after set: cloexec %v err %d", on, err)
	}
	if err := tab.SetCloexec(fd, false); err != 0 {
		t.Fatal(err)
	}
	if on, _ := tab.Cloexec(fd); on {
		t.Fatal("cloexec still set after clearing")
	}
	// A closed fd's flag does not survive into the next fd at that number.
	tab.SetCloexec(fd, true)
	tab.Close(fd)
	if _, err := tab.Cloexec(fd); err != wasiabi.EBADF {
		t.Fatalf("Cloexec on a closed fd: %d", err)
	}
	if err := tab.SetCloexec(fd, true); err != wasiabi.EBADF {
		t.Fatalf("SetCloexec on a closed fd: %d", err)
	}
	if again := install(t, tab, wasi.NewDesc(&countingFile{}, 0)); again != fd {
		t.Fatalf("reinstalled at %d", again)
	}
	if on, _ := tab.Cloexec(fd); on {
		t.Fatal("a new fd inherited the closed fd's cloexec")
	}
}

func TestDupFromTakesLowestFreeAtOrAboveMin(t *testing.T) {
	tab := wasi.NewFDTable()
	f := &countingFile{}
	tab.InstallAt(0, wasi.NewDesc(f, 0))
	tab.InstallAt(5, wasi.NewDesc(&countingFile{}, 0))
	tab.InstallAt(6, wasi.NewDesc(&countingFile{}, 0))
	got, err := tab.DupFrom(0, 5, true)
	if err != 0 || got != 7 {
		t.Fatalf("DupFrom(0, 5) = %d, %d; want 7", got, err)
	}
	if on, _ := tab.Cloexec(got); !on {
		t.Fatal("cloexec dup is not close-on-exec")
	}
	if on, _ := tab.Cloexec(0); on {
		t.Fatal("dup changed the original fd's flag")
	}
	low, err := tab.DupFrom(0, 0, false)
	if err != 0 || low != 1 {
		t.Fatalf("DupFrom(0, 0) = %d, %d; want 1", low, err)
	}
	if on, _ := tab.Cloexec(low); on {
		t.Fatal("plain dup is close-on-exec")
	}
	if d, _ := tab.Get(got); d.File() != f {
		t.Fatal("dup does not share the description")
	}
	tab.Close(0)
	tab.Close(1)
	if f.closes != 0 {
		t.Fatal("closed while a dup still holds it")
	}
	tab.Close(got)
	if f.closes != 1 {
		t.Fatalf("closes %d", f.closes)
	}
	if _, err := tab.DupFrom(9, 0, false); err != wasiabi.EBADF {
		t.Fatalf("DupFrom of a closed fd: %d", err)
	}
	if _, err := tab.DupFrom(5, -1, false); err != wasiabi.EINVAL {
		t.Fatalf("DupFrom with a negative min: %d", err)
	}
}

func TestDup2ReplacesAndReleases(t *testing.T) {
	tab := wasi.NewFDTable()
	f1, f2 := &countingFile{}, &countingFile{}
	tab.InstallAt(4, wasi.NewDesc(f1, 0))
	tab.InstallAt(9, wasi.NewDesc(f2, 0))
	tab.SetCloexec(9, true)
	if err := tab.Dup2(4, 9); err != 0 {
		t.Fatal(err)
	}
	if f2.closes != 1 {
		t.Fatal("the description newfd held was not released")
	}
	if d, _ := tab.Get(9); d.File() != f1 {
		t.Fatal("newfd does not refer to fd's description")
	}
	if on, _ := tab.Cloexec(9); on {
		t.Fatal("dup2's newfd is close-on-exec")
	}
	tab.Close(4)
	if f1.closes != 0 {
		t.Fatal("closed while newfd still holds it")
	}

	// dup2(fd, fd) changes nothing, not even the flag.
	tab.SetCloexec(9, true)
	if err := tab.Dup2(9, 9); err != 0 {
		t.Fatal(err)
	}
	if on, _ := tab.Cloexec(9); !on || f1.closes != 0 {
		t.Fatal("dup2(fd, fd) was not a no-op")
	}
	if err := tab.Dup2(4, 9); err != wasiabi.EBADF {
		t.Fatalf("dup2 from a closed fd: %d", err)
	}
	if err := tab.Dup2(9, -1); err != wasiabi.EBADF {
		t.Fatalf("dup2 to a negative fd: %d", err)
	}
	if err := tab.Dup2(4, 4); err != wasiabi.EBADF {
		t.Fatalf("dup2(fd, fd) of a closed fd: %d", err)
	}
}

func TestInheritInto(t *testing.T) {
	fs := memfs.New()
	parent := wasi.NewRootFDs(fs, nil, nil, nil)
	kept, dropped := &countingFile{}, &countingFile{}
	parent.InstallAt(20, wasi.NewDesc(kept, 0))
	parent.InstallAt(21, wasi.NewDesc(dropped, 0))
	parent.SetCloexec(21, true)
	parentPreopen, _ := parent.Get(3)

	child := wasi.NewChildFDs(fs)
	childPreopen, _ := child.Get(3)
	parent.InheritInto(child)

	if d, _ := child.Get(3); d != childPreopen || d == parentPreopen {
		t.Fatal("fd 3 was inherited over the child's own preopen")
	}
	if d, err := child.Get(20); err != 0 || d.File() != kept {
		t.Fatal("fd 20 not inherited at the same number")
	}
	if on, _ := child.Cloexec(20); on {
		t.Fatal("inherited fd is close-on-exec")
	}
	if _, err := child.Get(21); err != wasiabi.EBADF {
		t.Fatal("close-on-exec fd 21 reached the child")
	}
	for fd := int32(0); fd < 3; fd++ {
		if _, err := child.Get(fd); err != 0 {
			t.Fatalf("stdio fd %d not inherited", fd)
		}
	}
	if child.Len() != 5 {
		t.Fatalf("child has %d fds, want 0-3 and 20", child.Len())
	}

	parent.CloseAll()
	if kept.closes != 0 {
		t.Fatal("closing the parent's copy closed the child's")
	}
	if dropped.closes != 1 {
		t.Fatalf("close-on-exec description: closes %d", dropped.closes)
	}
	child.CloseAll()
	if kept.closes != 1 {
		t.Fatalf("shared description: closes %d", kept.closes)
	}
}

// TestFDCeiling checks that every fd stays below MaxFiles, the
// RLIMIT_NOFILE the compat layer reports.
func TestFDCeiling(t *testing.T) {
	tab := wasi.NewFDTable()
	f := &countingFile{}
	tab.InstallAt(0, wasi.NewDesc(f, 0))

	if err := tab.Dup2(0, wasi.MaxFiles); err != wasiabi.EBADF {
		t.Fatalf("Dup2 to MaxFiles: %d, want EBADF", err)
	}
	if err := tab.Dup2(0, wasi.MaxFiles-1); err != wasiabi.ESUCCESS {
		t.Fatalf("Dup2 to MaxFiles-1: %d", err)
	}
	if _, err := tab.DupFrom(0, wasi.MaxFiles, false); err != wasiabi.EINVAL {
		t.Fatalf("DupFrom min MaxFiles: %d, want EINVAL", err)
	}
	// MaxFiles-1 is taken, so nothing is free from there up.
	if _, err := tab.DupFrom(0, wasi.MaxFiles-1, false); err != wasiabi.EMFILE {
		t.Fatalf("DupFrom with no free fd: %d, want EMFILE", err)
	}
	if err := tab.Renumber(0, wasi.MaxFiles); err != wasiabi.EBADF {
		t.Fatalf("Renumber to MaxFiles: %d, want EBADF", err)
	}

	// Fill the table. One more Install fails and closes what it was given.
	for tab.Len() < wasi.MaxFiles {
		if _, err := tab.DupFrom(0, 0, false); err != wasiabi.ESUCCESS {
			t.Fatalf("DupFrom with %d open: %d", tab.Len(), err)
		}
	}
	extra := &countingFile{}
	if fd, err := tab.Install(wasi.NewDesc(extra, 0)); err != wasiabi.EMFILE {
		t.Fatalf("Install into a full table: fd %d, %d; want EMFILE", fd, err)
	}
	if extra.closes != 1 {
		t.Fatalf("refused description closed %d times, want 1", extra.closes)
	}
	tab.CloseAll()
	if f.closes != 1 {
		t.Fatalf("shared description closed %d times, want 1", f.closes)
	}
}
