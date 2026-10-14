package pgtest

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/feverenergy/pgmem/internal/rss"
)

// The RSS guard stops a runaway test binary before it takes the machine
// down. From the first server start on, a goroutine samples the test
// process's resident memory (PSS on Linux) every guardPeriod. Past
// PGTEST_MAX_RSS_MB, it writes a message and the end of every server's log
// to stderr and exits with code 3. The default limit is defaultMaxRSSMiB,
// and 0 turns the guard off. If guardFailures samples in a row fail, the
// guard writes one warning and stops, and the test binary keeps running
// without it. pgmem-serve's watchdog stops the server in that case
// instead.
//
// These are variables only so that tests can replace them. The guard
// copies them when it starts.
var (
	guardSample           = rss.Resident
	guardExit             = os.Exit
	guardStderr io.Writer = os.Stderr
	guardPeriod           = 500 * time.Millisecond
)

const (
	defaultMaxRSSMiB = 4096
	guardFailures    = 4
	guardExitCode    = 3
)

// A guard is the running RSS guard.
type guard struct {
	limitMiB int64
	sample   func() (int64, error)
	exit     func(int)
	stderr   io.Writer
	period   time.Duration
	quit     chan struct{}
	done     chan struct{}
}

// guardState holds the process's guard. The first startGuard sets started,
// even when the guard is off, so that startGuard runs once.
var guardState struct {
	mu      sync.Mutex
	started bool
	g       *guard // nil when off, or not started
}

// startGuard starts the RSS guard, once per process (per resetForTest in
// this package's tests).
func startGuard() {
	guardState.mu.Lock()
	defer guardState.mu.Unlock()
	if guardState.started {
		return
	}
	guardState.started = true
	limit := maxRSSMiB(guardStderr)
	if limit == 0 {
		return
	}
	g := &guard{
		limitMiB: limit,
		sample:   guardSample,
		exit:     guardExit,
		stderr:   guardStderr,
		period:   guardPeriod,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	guardState.g = g
	go g.loop()
}

// maxRSSMiB returns PGTEST_MAX_RSS_MB, or defaultMaxRSSMiB if it is unset.
// If the value is not a whole number of MiB from 0 to math.MaxInt64>>20,
// maxRSSMiB reports it on w and returns the default. Past that maximum,
// the limit in bytes overflows.
func maxRSSMiB(w io.Writer) int64 {
	v, ok := os.LookupEnv("PGTEST_MAX_RSS_MB")
	if !ok || v == "" {
		return defaultMaxRSSMiB
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64>>20 {
		fmt.Fprintf(w, "pgtest: PGTEST_MAX_RSS_MB=%q is not a number of MiB; using %d\n", v, defaultMaxRSSMiB)
		return defaultMaxRSSMiB
	}
	return n
}

func (g *guard) loop() {
	defer close(g.done)
	tk := time.NewTicker(g.period)
	defer tk.Stop()
	failures := 0
	for {
		v, err := g.sample()
		switch {
		case err != nil:
			failures++
			if failures >= guardFailures {
				fmt.Fprintf(g.stderr, "pgtest: cannot read the test process's resident memory (%d samples in a row failed: %v); the PGTEST_MAX_RSS_MB guard stops\n", failures, err)
				return
			}
		case v > g.limitMiB<<20:
			g.trip(v)
			return
		default:
			failures = 0
		}
		select {
		case <-tk.C:
		case <-g.quit:
			return
		}
	}
}

// trip writes why the process exits and the end of every server's log,
// then exits.
func (g *guard) trip(v int64) {
	fmt.Fprintf(g.stderr, "pgtest: test process resident memory %d MiB exceeds PGTEST_MAX_RSS_MB=%d; aborting\n", v>>20, g.limitMiB)
	registry.mu.Lock()
	keys := slices.SortedFunc(maps.Keys(registry.servers), func(a, b serverKey) int {
		return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.buildID, b.buildID))
	})
	servers := make([]*server, len(keys))
	for i, k := range keys {
		servers[i] = registry.servers[k]
	}
	registry.mu.Unlock()
	for i, s := range servers {
		s.mu.Lock()
		srv := s.srv
		s.mu.Unlock()
		if srv == nil {
			continue // still starting, or failed to
		}
		lines := strings.Split(strings.TrimRight(srv.Log(), "\n"), "\n")
		lines = lines[max(0, len(lines)-maxLogLines):]
		fmt.Fprintf(g.stderr, "pgtest: last %d lines of the server log for template %q (engine %s):\n%s\n", len(lines), keys[i].key, s.engine, strings.Join(lines, "\n"))
	}
	g.exit(guardExitCode)
}

// stopGuard stops the guard and waits for it, for at most a sample's
// timeout plus a margin. The next startGuard starts it again. Only tests
// stop it.
func stopGuard() {
	guardState.mu.Lock()
	g := guardState.g
	guardState.g, guardState.started = nil, false
	guardState.mu.Unlock()
	if g == nil {
		return
	}
	close(g.quit)
	select {
	case <-g.done:
	case <-time.After(rss.Timeout + time.Second):
	}
}
