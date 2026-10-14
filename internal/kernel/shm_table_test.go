package kernel

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// testMem is a 4 MiB linear memory from a, freed at cleanup unless the
// test frees it first.
func testMem(t *testing.T, a *vmem.Allocator) ([]byte, func()) {
	t.Helper()
	m := a.Allocate(0, 64<<20)
	t.Cleanup(m.Free)
	return m.Reallocate(4 << 20), m.Free
}

func mustGet(t *testing.T, tb *shmTable, key int32, size uint32, flags int32) int32 {
	t.Helper()
	id, errno := tb.get(100, key, size, flags)
	if errno != wasiabi.ESUCCESS {
		t.Fatalf("get(%d, %d, %#o): %v", key, size, flags, errno)
	}
	return id
}

func TestShmTableGet(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	p1 := mustGet(t, tb, 0, 4096, ipcCreat)
	p2 := mustGet(t, tb, 0, 4096, ipcCreat)
	if p1 == p2 || p1 <= 0 {
		t.Fatalf("private ids %d %d", p1, p2)
	}
	k := mustGet(t, tb, 77, 8192, ipcCreat|ipcExcl)
	cases := []struct {
		key   int32
		size  uint32
		flags int32
		want  wasiabi.Errno
	}{
		{77, 8192, ipcCreat | ipcExcl, wasiabi.EEXIST},
		{77, 16384, 0, wasiabi.EINVAL}, // larger than the segment
		{78, 4096, 0, wasiabi.ENOENT},
		{0, 0, ipcCreat, wasiabi.EINVAL},
		{0, maxShmSize + 1, ipcCreat, wasiabi.EINVAL},
	}
	for _, c := range cases {
		if _, errno := tb.get(100, c.key, c.size, c.flags); errno != c.want {
			t.Errorf("get(%d, %d, %#o) = %v, want %v", c.key, c.size, c.flags, errno, c.want)
		}
	}
	if got := mustGet(t, tb, 77, 100, 0); got != k {
		t.Fatalf("lookup by key: %d, want %d", got, k)
	}
	if got := mustGet(t, tb, 77, 100, ipcCreat); got != k {
		t.Fatalf("IPC_CREAT without IPC_EXCL: %d, want %d", got, k)
	}
}

// Ids stay positive, because a negative result is -errno. Once they run
// out, new segments are refused while existing keys still resolve.
func TestShmTableIDsRunOut(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	k := mustGet(t, tb, 5, 4096, ipcCreat)
	tb.nextID = math.MaxInt32 - 1
	if last := mustGet(t, tb, 0, 4096, ipcCreat); last != math.MaxInt32-1 {
		t.Fatalf("last id %d", last)
	}
	for _, key := range []int32{0, 6} {
		if id, e := tb.get(100, key, 4096, ipcCreat); e != wasiabi.ENOSPC {
			t.Fatalf("get(%d) past the last id: %d %v", key, id, e)
		}
	}
	if got := mustGet(t, tb, 5, 4096, 0); got != k {
		t.Fatalf("lookup by key: %d, want %d", got, k)
	}
}

func TestShmTableAttachShares(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	m1, _ := testMem(t, &a)
	m2, _ := testMem(t, &a)
	id := mustGet(t, tb, 0, 100000, ipcCreat)
	if e := tb.attach(1, m1, id, 1<<20); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	if e := tb.attach(2, m2, id, 2<<20); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	m1[1<<20+99999] = 5
	if m2[2<<20+99999] != 5 {
		t.Fatal("not shared")
	}
	st, e := tb.stat(id)
	if e != wasiabi.ESUCCESS || st.nattch != 2 || st.size != 100000 || st.mapped != 7*vmem.PageAlign || st.cpid != 100 || st.lpid != 2 {
		t.Fatalf("stat %+v %v", st, e)
	}
	for _, c := range []struct {
		name string
		mem  []byte
		id   int32
		addr uint32
		want wasiabi.Errno
	}{
		{"no such id", m1, 9999, 3 << 20, wasiabi.EINVAL},
		{"null address", m1, id, 0, wasiabi.EINVAL},
		{"unaligned", m1, id, 3<<20 + 4096, wasiabi.EINVAL},
		{"overlaps an attachment", m1, id, 1<<20 + vmem.PageAlign, wasiabi.EINVAL},
		{"past the memory", m1, id, 4<<20 - vmem.PageAlign, wasiabi.EINVAL},
		{"not a vmem memory", make([]byte, 4<<20), id, 1 << 20, wasiabi.ENOMEM},
	} {
		if e := tb.attach(1, c.mem, c.id, c.addr); e != c.want {
			t.Errorf("%s: %v, want %v", c.name, e, c.want)
		}
	}
	if e := tb.detach(1, m1, 3<<20); e != wasiabi.EINVAL {
		t.Fatalf("detach of nothing: %v", e)
	}
	if e := tb.detach(1, m1, 1<<20); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	if m1[1<<20+99999] != 0 {
		t.Fatal("detached range still shows the segment")
	}
	if st, _ := tb.stat(id); st.nattch != 1 || st.lpid != 1 {
		t.Fatalf("after detach %+v", st)
	}
}

// A removed segment loses its key at once and is freed at its last detach.
// Ids are never reused.
func TestShmTableRemoveLifetime(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	var freed []int32
	tb.onFree = func(id int32) { freed = append(freed, id) }
	m, _ := testMem(t, &a)
	id := mustGet(t, tb, 55, 4096, ipcCreat)
	if e := tb.attach(1, m, id, 1<<20); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	if e := tb.rmid(id); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	if _, e := tb.get(1, 55, 4096, 0); e != wasiabi.ENOENT {
		t.Fatalf("removed key still found: %v", e)
	}
	if st, e := tb.stat(id); e != wasiabi.ESUCCESS || st.key != 0 || st.nattch != 1 {
		t.Fatalf("stat of removed, attached segment: %+v %v", st, e)
	}
	m[1<<20] = 1 // still mapped and usable
	if segs, _ := tb.stats(); segs != 1 || len(freed) != 0 {
		t.Fatalf("freed while attached: %d segments, freed %v", segs, freed)
	}
	if e := tb.detach(1, m, 1<<20); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	if _, e := tb.stat(id); e != wasiabi.EINVAL {
		t.Fatalf("stat after free: %v", e)
	}
	if len(freed) != 1 || freed[0] != id {
		t.Fatalf("onFree %v", freed)
	}
	if again := mustGet(t, tb, 55, 4096, ipcCreat); again == id {
		t.Fatal("id reused")
	}
	if e := tb.rmid(id); e != wasiabi.EINVAL {
		t.Fatalf("rmid of freed segment: %v", e)
	}
}

// release runs at a process's exit. It drops the process's attachments
// without touching its memory, which may already be freed.
func TestShmTableRelease(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	m, free := testMem(t, &a)
	m8, _ := testMem(t, &a)
	keep := mustGet(t, tb, 0, 4096, ipcCreat)
	gone := mustGet(t, tb, 0, 4096, ipcCreat)
	for i, id := range []int32{keep, gone} {
		if e := tb.attach(7, m, id, uint32(1+i)<<20); e != wasiabi.ESUCCESS {
			t.Fatal(e)
		}
	}
	if e := tb.attach(8, m8, keep, 1<<20); e != wasiabi.ESUCCESS { // lpid 8
		t.Fatal(e)
	}
	if e := tb.rmid(gone); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	free()
	tb.release(7)
	if segs, att := tb.stats(); segs != 1 || att != 1 {
		t.Fatalf("after release: %d segments, %d attachments", segs, att)
	}
	// Exit detaches, so the exiting process becomes the last to operate.
	if st, _ := tb.stat(keep); st.nattch != 1 || st.lpid != 7 {
		t.Fatalf("after release: %+v", st)
	}
	tb.release(7) // nothing left, so a no-op
	tb.release(8)
}

func TestShmTableClose(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	n := 0
	tb.onFree = func(int32) { n++ }
	mustGet(t, tb, 0, 4096, ipcCreat)
	mustGet(t, tb, 9, 4096, ipcCreat)
	tb.close()
	if segs, att := tb.stats(); segs != 0 || att != 0 || n != 2 {
		t.Fatalf("after close: %d segments, %d attachments, %d freed", segs, att, n)
	}
	if _, e := tb.get(1, 0, 4096, ipcCreat); e != wasiabi.ENOMEM {
		t.Fatalf("get after close: %v", e)
	}
}

func TestShmTableLocate(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	m, _ := testMem(t, &a)
	id := mustGet(t, tb, 0, 4096, ipcCreat)
	if e := tb.attach(3, m, id, 1<<20); e != wasiabi.ESUCCESS {
		t.Fatal(e)
	}
	if seg, off, ok := tb.locate(3, 1<<20+32, 16); !ok || seg != id || off != 32 {
		t.Fatalf("locate: %d %d %v", seg, off, ok)
	}
	if _, _, ok := tb.locate(3, 1<<20+vmem.PageAlign-8, 16); ok {
		t.Fatal("located a range that runs past the mapping")
	}
	if _, _, ok := tb.locate(4, 1<<20+32, 16); ok {
		t.Fatal("located another process's attachment")
	}
}

// Concurrent processes create, attach and remove segments. -race checks
// the locking.
func TestShmTableConcurrent(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	var wg sync.WaitGroup
	for pid := int32(1); pid <= 4; pid++ {
		m, _ := testMem(t, &a)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				id, e := tb.get(pid, 0, 4096, ipcCreat)
				if e != wasiabi.ESUCCESS {
					t.Error(e)
					return
				}
				if e := tb.attach(pid, m, id, 1<<20); e != wasiabi.ESUCCESS {
					t.Error(e)
					return
				}
				tb.rmid(id)
				if e := tb.detach(pid, m, 1<<20); e != wasiabi.ESUCCESS {
					t.Error(e)
					return
				}
			}
		}()
	}
	wg.Wait()
	if segs, att := tb.stats(); segs != 0 || att != 0 {
		t.Fatalf("%d segments, %d attachments left", segs, att)
	}
}

// When the segment's object cannot be made, get answers ENOMEM and passes
// the reason, naming the error, to report for the calling process.
func TestShmTableGetLogsNewSharedFailure(t *testing.T) {
	var a vmem.Allocator
	tb := newShmTable(&a)
	defer tb.close()
	boom := errors.New("injected mmap failure")
	tb.newShared = func(uint64) (*vmem.Shared, error) { return nil, boom }
	var gotPID int32
	var lines []string
	tb.report = func(pid int32, msg string) { gotPID = pid; lines = append(lines, msg) }
	if _, e := tb.get(100, 0, 4096, ipcCreat); e != wasiabi.ENOMEM {
		t.Fatalf("get: %v, want ENOMEM", e)
	}
	if gotPID != 100 || len(lines) != 1 || !strings.Contains(lines[0], boom.Error()) || !strings.HasSuffix(lines[0], "\n") {
		t.Fatalf("pid %d, report lines %q", gotPID, lines)
	}
	// Other failures do not report, and nothing was left behind.
	if _, e := tb.get(100, 0, 0, ipcCreat); e != wasiabi.EINVAL || len(lines) != 1 {
		t.Fatalf("EINVAL case: %v, %d lines", e, len(lines))
	}
	if n, _ := tb.stats(); n != 0 {
		t.Fatalf("%d segments after a failed get", n)
	}
}
