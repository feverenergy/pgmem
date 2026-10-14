package kernel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/feverenergy/pgmem/internal/kernel"
)

// wrongABI imports pgm.getpid. It exports an empty _start and a
// pgmem_abi_version that returns 1, an older ABI. So it passes Register but
// fails the run-time check.
var wrongABI = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// types: () -> i32, () -> ()
	0x01, 0x08, 0x02, 0x60, 0x00, 0x01, 0x7f, 0x60, 0x00, 0x00,
	// import pgm.getpid: type 0
	0x02, 0x0e, 0x01, 0x03, 'p', 'g', 'm', 0x06, 'g', 'e', 't', 'p', 'i', 'd', 0x00, 0x00,
	// functions 1 and 2: types 0 and 1
	0x03, 0x03, 0x02, 0x00, 0x01,
	// exports
	0x07, 0x1e, 0x02,
	0x11, 'p', 'g', 'm', 'e', 'm', '_', 'a', 'b', 'i', '_', 'v', 'e', 'r', 's', 'i', 'o', 'n', 0x00, 0x01,
	0x06, '_', 's', 't', 'a', 'r', 't', 0x00, 0x02,
	// code: i32.const 1; and nothing
	0x0a, 0x09, 0x02, 0x04, 0x00, 0x41, 0x01, 0x0b, 0x02, 0x00, 0x0b,
}

func TestStartRefusesOtherABIVersion(t *testing.T) {
	k := newKernel(t)
	m, err := k.Compile(context.Background(), wrongABI)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Register("/fixtures/wrongabi", m); err != nil {
		t.Fatal(err)
	}
	_, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/wrongabi"})
	if !st.Exited() || st.ExitCode() != 127 {
		t.Fatalf("status %#x", st)
	}
	if !strings.Contains(errOut, "ABI version 1; this kernel runs version 5") {
		t.Fatalf("stderr %q", errOut)
	}
}

// A module whose start section traps. It compiles and registers, but it
// cannot be instantiated.
var trapsOnInstantiate = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type: () -> ()
	0x03, 0x03, 0x02, 0x00, 0x00, // two functions
	0x07, 0x0a, 0x01, 0x06, '_', 's', 't', 'a', 'r', 't', 0x00, 0x01, // export _start = func 1
	0x08, 0x01, 0x00, // start = func 0
	0x0a, 0x08, 0x02, 0x03, 0x00, 0x00, 0x0b, 0x02, 0x00, 0x0b, // func 0: unreachable
}

func TestStartReturnsInstantiateError(t *testing.T) {
	k := newKernel(t)
	m, err := k.Compile(context.Background(), trapsOnInstantiate)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Register("/fixtures/badstart", m); err != nil {
		t.Fatal(err)
	}
	// With no Stderr, the reason must reach the caller as an error.
	p, err := k.Start(context.Background(), kernel.Spec{Path: "/fixtures/badstart"})
	if err == nil || p != nil {
		t.Fatalf("Start = %v, %v; want an error", p, err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("error %q does not say why", err)
	}
}
