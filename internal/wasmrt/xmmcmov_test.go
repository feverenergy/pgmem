package wasmrt_test

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"

	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// xmmcmovWasm reproduces wazero issue #2534. v1.12.0's amd64 backend
// lowers a float select to "tmp = y; xmmcmov(c) x -> tmp" but declares the
// cmov as not reading tmp. So the register allocator can drop the copy of
// y, and the select returns a stale register when c is false.
// postgres.wasm hit it in initial_cost_nestloop, so amd64 plans differed
// from arm64.
//
//	(func $sub (param f64 f64) (result f64) (f64.sub (local.get 1) (local.get 0)))
//	(func (export "f") (param $c i32) (param $a f64) (param $b f64) (result f64)
//	  (local $x f64)
//	  (local.tee $x (f64.add (local.get $a) (local.get $a)))  ;; select's x
//	  (call $sub (local.get $x) (local.get $b))               ;; y = b - x
//	  local.get $c
//	  select)                                                 ;; c ? x : y
//
// x must be a local, not a parameter. x is reloaded after the call into
// the register it was passed in, which is the register holding tmp.
var xmmcmovWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: (f64 f64)->f64, (i32 f64 f64)->f64
	0x01, 0x0e, 0x02,
	0x60, 0x02, 0x7c, 0x7c, 0x01, 0x7c,
	0x60, 0x03, 0x7f, 0x7c, 0x7c, 0x01, 0x7c,
	// function section
	0x03, 0x03, 0x02, 0x00, 0x01,
	// export "f" = func 1
	0x07, 0x05, 0x01, 0x01, 'f', 0x00, 0x01,
	// code section
	0x0a, 0x1e, 0x02,
	0x07, 0x00, 0x20, 0x01, 0x20, 0x00, 0xa1, 0x0b, // sub: local.get 1, local.get 0, f64.sub
	0x14, 0x01, 0x01, 0x7c, // f: one f64 local ($x)
	0x20, 0x01, 0x20, 0x01, 0xa0, // a + a
	0x22, 0x03, // local.tee $x
	0x20, 0x03, // local.get $x (first call argument)
	0x20, 0x02, // local.get b
	0x10, 0x00, // call sub (y = b - x)
	0x20, 0x00, // local.get c
	0x1b, // select
	0x0b,
}

// TestXmmCMovFloatSelect fails on linux/amd64 with wazero v1.12.0's
// compiler, where f(0, 3, 10) returns 6 instead of 4.
func TestXmmCMovFloatSelect(t *testing.T) {
	for _, interp := range []bool{false, true} {
		name := map[bool]string{false: "compiler", true: "interpreter"}[interp]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, err := wasmrt.NewRuntime(ctx, wasmrt.Options{Interpreter: interp})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, xmmcmovWasm)
			if err != nil {
				t.Fatal(err)
			}
			f := mod.ExportedFunction("f")
			for _, tc := range []struct {
				c    uint64
				want float64
			}{{0, 4}, {1, 6}} {
				res, err := f.Call(ctx, tc.c, api.EncodeF64(3), api.EncodeF64(10))
				if err != nil {
					t.Fatal(err)
				}
				if got := api.DecodeF64(res[0]); got != tc.want {
					t.Errorf("f(%d, 3, 10) = %v, want %v", tc.c, got, tc.want)
				}
			}
		})
	}
}
