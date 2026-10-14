package wasmrt_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"golang.org/x/sys/cpu"

	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// memclrWasm zero-fills 1500 bytes with a float in xmm15. wazero main from
// 9538c53e (#2538, the memory.fill rewrite) lowers a zero memory.fill of
// 1 KiB or more to a direct call of runtime.memclrNoHeapPointers. That is
// ABIInternal assembly. Without AVX2 it clears with SSE stores of X15, Go's
// zero register, but wazero's code leaves xmm15 holding anything. Its entry
// preamble loads stack-passed float arguments through xmm15. So ten f64
// parameters leave the last one there at the fill, and the "cleared" memory
// held its bytes. pgmem pins a commit before 9538c53e (docs/architecture.md).
//
//	(memory (export "memory") 1)
//	(func (export "f") (param f64 f64 f64 f64 f64 f64 f64 f64 f64 f64) (result f64)
//	  (memory.fill (i32.const 0) (i32.const 0) (i32.const 1500))
//	  (local.get 9))
var memclrWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: (f64 x10) -> f64
	0x01, 0x0f, 0x01, 0x60, 0x0a,
	0x7c, 0x7c, 0x7c, 0x7c, 0x7c, 0x7c, 0x7c, 0x7c, 0x7c, 0x7c,
	0x01, 0x7c,
	// function section
	0x03, 0x02, 0x01, 0x00,
	// memory section: one page
	0x05, 0x03, 0x01, 0x00, 0x01,
	// export section: "f" = func 0, "memory" = memory 0
	0x07, 0x0e, 0x02,
	0x01, 'f', 0x00, 0x00,
	0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00,
	// code section
	0x0a, 0x10, 0x01, 0x0e, 0x00,
	0x41, 0x00, // i32.const 0 (dst)
	0x41, 0x00, // i32.const 0 (value)
	0x41, 0xdc, 0x0b, // i32.const 1500 (len)
	0xfc, 0x0b, 0x00, // memory.fill
	0x20, 0x09, // local.get 9
	0x0b,
}

// zeroFill runs memclrWasm on r over 0xaa bytes, with pi as the tenth
// argument, and returns the 1500 bytes the fill should have cleared.
func zeroFill(t *testing.T, r wazero.Runtime) []byte {
	t.Helper()
	ctx := context.Background()
	mod, err := r.Instantiate(ctx, memclrWasm)
	if err != nil {
		t.Fatal(err)
	}
	if !mod.Memory().Write(0, bytes.Repeat([]byte{0xaa}, 2048)) {
		t.Fatal("write failed")
	}
	args := make([]uint64, 10)
	for i := range args {
		args[i] = api.EncodeF64(float64(i+1) * 1.25)
	}
	args[9] = api.EncodeF64(math.Pi)
	if _, err := mod.ExportedFunction("f").Call(ctx, args...); err != nil {
		t.Fatal(err)
	}
	got, _ := mod.Memory().Read(0, 2048)
	if !bytes.Equal(got[1500:], bytes.Repeat([]byte{0xaa}, 548)) {
		t.Fatal("memory.fill wrote past its end")
	}
	return got[:1500]
}

// avx2OffEnv marks the child run of TestZeroFillClears.
const avx2OffEnv = "PGMEM_TEST_ZEROFILL_AVX2_OFF"

// TestZeroFillClears guards a wazero update against bringing back the X15
// bug. A zero memory.fill of 1 KiB or more must clear memory on the
// compiler and the interpreter. On amd64 the test runs itself again with
// GODEBUG=cpu.avx2=off,cpu.erms=off. That makes the Go runtime's memclr
// take the SSE path that stores X15, so AVX2 hosts and make
// test-linux-amd64 cover CPUs without AVX2. Under qemu, QEMU_CPU=Nehalem
// does the same for real.
func TestZeroFillClears(t *testing.T) {
	t.Logf("GOARCH=%s AVX2=%v ERMS=%v", runtime.GOARCH, cpu.X86.HasAVX2, cpu.X86.HasERMS)
	for _, interp := range []bool{false, true} {
		t.Run(map[bool]string{false: "compiler", true: "interpreter"}[interp], func(t *testing.T) {
			ctx := context.Background()
			r, err := wasmrt.NewRuntime(ctx, wasmrt.Options{Interpreter: interp})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close(ctx)
			if got := zeroFill(t, r); !bytes.Equal(got, make([]byte, len(got))) {
				t.Fatalf("memory.fill 0 left non-zero bytes (AVX2=%v); first 16: % x", cpu.X86.HasAVX2, got[:16])
			}
		})
	}
	if runtime.GOARCH != "amd64" || os.Getenv(avx2OffEnv) != "" {
		return
	}
	t.Run("avx2=off", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestZeroFillClears$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), avx2OffEnv+"=1", "GODEBUG=cpu.avx2=off,cpu.erms=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !bytes.Contains(out, []byte("AVX2=false ERMS=false")) {
			t.Fatalf("GODEBUG did not turn AVX2 and ERMS off:\n%s", out)
		}
	})
}
