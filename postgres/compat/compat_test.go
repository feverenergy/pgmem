// Package compat holds the test that runs the compat layer's C test inside
// WebAssembly. The layer itself is C, in include/ and src/.
package compat

import (
	"os"
	"testing"

	"github.com/feverenergy/pgmem/internal/wasmrt/wasmrttest"
)

func TestCompatLayer(t *testing.T) {
	wasm, err := os.ReadFile("testdata/compat_test.wasm")
	if err != nil {
		t.Fatalf("%v (run make fixtures)", err)
	}
	res := wasmrttest.Run(t, wasm, wasmrttest.Options{}, "/compat_test")
	if !res.Status.Exited() || res.Status.ExitCode() != 0 || res.Stdout != "compat ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", res.Status, res.Stdout, res.Stderr)
	}
}

func TestShell(t *testing.T) {
	wasm, err := os.ReadFile("testdata/shell_test.wasm")
	if err != nil {
		t.Fatalf("%v (run make fixtures)", err)
	}
	res := wasmrttest.Run(t, wasm, wasmrttest.Options{}, "/fixtures/shell")
	if !res.Status.Exited() || res.Status.ExitCode() != 0 || res.Stdout != "shell ok\n" || res.Stderr != "" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", res.Status, res.Stdout, res.Stderr)
	}
}
