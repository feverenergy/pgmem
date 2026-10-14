package vmem_test

import (
	"context"
	"os"
	"runtime"
	"testing"
	"unsafe"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

func TestGrowKeepsBaseAndZeroes(t *testing.T) {
	var a vmem.Allocator
	m := a.Allocate(0, 64<<20)
	defer m.Free()

	b1 := m.Reallocate(1 << 16)
	if len(b1) != 1<<16 {
		t.Fatalf("len %d", len(b1))
	}
	b1[0], b1[len(b1)-1] = 1, 2
	b2 := m.Reallocate(3 << 20)
	if len(b2) != 3<<20 {
		t.Fatalf("len %d", len(b2))
	}
	if unsafe.SliceData(b1) != unsafe.SliceData(b2) {
		t.Fatal("base address moved")
	}
	if b2[0] != 1 || b2[1<<16-1] != 2 || b2[3<<20-1] != 0 {
		t.Fatal("contents not preserved or new pages not zero")
	}
	if got := a.Stats(); got.Live != 1 || got.Committed < 3<<20 {
		t.Fatalf("stats %+v", got)
	}
}

func TestReallocateBeyondMaxFails(t *testing.T) {
	var a vmem.Allocator
	m := a.Allocate(0, 1<<20)
	defer m.Free()
	if m.Reallocate(2<<20) != nil {
		t.Fatal("grew past the reservation")
	}
}

func TestFreeReleases(t *testing.T) {
	var a vmem.Allocator
	m := a.Allocate(0, 16<<20)
	m.Reallocate(8 << 20)
	m.Free()
	m.Free() // second Free is a no-op
	if got := a.Stats(); got.Live != 0 || got.Committed != 0 || got.PeakCommitted < 8<<20 {
		t.Fatalf("stats %+v", got)
	}
}

// Each fixture instance declares a 4 GiB maximum memory. With wazero's
// default allocator 64 instances drive the Go heap to many GiB. The fixture
// is a plain WASI program that never links the compat layer, so it runs on
// wazero's WASI without the pgm module.
func TestManyInstancesStaySmall(t *testing.T) {
	wasm, err := os.ReadFile("../wasi/testdata/exit.wasm")
	if err != nil {
		t.Fatal(err)
	}
	var a vmem.Allocator
	ctx := experimental.WithMemoryAllocator(context.Background(), &a)
	r, err := wasmrt.NewRuntime(ctx, wasmrt.Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	m, err := r.CompileModule(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		mod, err := r.InstantiateModule(ctx, m, wazero.NewModuleConfig().WithName(""))
		if err != nil {
			t.Fatal(err)
		}
		_ = mod.Close(ctx)
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapSys > 512<<20 {
		t.Fatalf("Go heap reserved %d MiB; instances are not using the mmap allocator", ms.HeapSys>>20)
	}
	got := a.Stats()
	if got.PeakCommitted == 0 {
		t.Fatal("the instances never used the allocator")
	}
	if got.Live != 0 {
		t.Fatalf("%d memories not freed", got.Live)
	}
}
