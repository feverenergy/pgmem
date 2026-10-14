package kernel_test

import (
	"testing"

	"github.com/feverenergy/pgmem/internal/kernel"
)

// TestFdsFixture runs testdata/c/fds.c. A spawned child inherits the fds
// that are not close-on-exec, at the same numbers, and none of those that
// are. It also inherits the parent's working directory.
func TestFdsFixture(t *testing.T) {
	k := newKernel(t, "fds", "bare")
	out, errOut, st := run(t, k, kernel.Spec{Path: "/fixtures/fds"})
	if !st.Exited() || st.ExitCode() != 0 || out != "fds ok\n" {
		t.Fatalf("status %#x\nstdout:\n%s\nstderr:\n%s", st, out, errOut)
	}
}
