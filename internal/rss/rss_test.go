package rss

import (
	"os"
	"testing"
)

func TestParsePSS(t *testing.T) {
	rollup := "55d0c0a6e000-7ffd2e5f8000 ---p 00000000 00:00 0                          [rollup]\n" +
		"Rss:              412340 kB\nPss:              301234 kB\nPss_Dirty:        200000 kB\nShared_Clean:      10000 kB\n"
	if v, err := parsePSS([]byte(rollup)); err != nil || v != 301234<<10 {
		t.Fatalf("parsePSS = %d, %v; want %d", v, err, 301234<<10)
	}
	for _, bad := range []string{"Rss: 1 kB\n", "Pss: x kB\n", "Pss: 12\n"} {
		if v, err := parsePSS([]byte(bad)); err == nil {
			t.Errorf("parsePSS(%q) = %d, want an error", bad, v)
		}
	}
	// On Linux Resident reads it for real.
	if b, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		if v, err := parsePSS(b); err != nil || v <= 0 {
			t.Fatalf("this process's PSS: %d, %v", v, err)
		}
	}
}

// TestResident reads this process's resident memory, which a running Go
// test holds at least a megabyte of, and far less than a terabyte.
func TestResident(t *testing.T) {
	v, err := Resident()
	if err != nil {
		t.Fatal(err)
	}
	if v < 1<<20 || v > 1<<40 {
		t.Fatalf("Resident() = %d bytes", v)
	}
}
