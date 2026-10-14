// Package rss reads this process's resident memory, for the RSS watchdogs
// of cmd/pgmem-serve and pgtest.
package rss

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Timeout bounds one ps(1) run, so a hung sample cannot stall a watchdog
// for longer.
const Timeout = time.Second

// Resident returns this process's resident set size in bytes. On Linux it
// is the proportional set size from /proc/self/smaps_rollup, or else the
// RSS from /proc/self/statm. Neither needs a fork. The proportional set
// size counts a page mapped twice only once, such as the shared memory
// every guest process maps. Elsewhere, as on macOS, Resident runs /bin/ps
// by absolute path, bounded by Timeout.
func Resident() (int64, error) {
	if b, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		return parsePSS(b)
	}
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		f := strings.Fields(string(b))
		if len(f) < 2 {
			return 0, fmt.Errorf("/proc/self/statm: %q", b)
		}
		pages, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/self/statm: %w", err)
		}
		return pages * int64(os.Getpagesize()), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, fmt.Errorf("/bin/ps: %w", err)
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("/bin/ps: %w", err)
	}
	return kib << 10, nil
}

// parsePSS returns the "Pss:" line of /proc/self/smaps_rollup in bytes.
func parsePSS(b []byte) (int64, error) {
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "Pss:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 2 || f[1] != "kB" {
			return 0, fmt.Errorf("/proc/self/smaps_rollup: %q", line)
		}
		kib, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/self/smaps_rollup: %w", err)
		}
		return kib << 10, nil
	}
	return 0, errors.New("/proc/self/smaps_rollup: no Pss line")
}
