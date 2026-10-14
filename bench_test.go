package pgmem_test

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/feverenergy/pgmem"
)

// Benchmarks compare the default with wazero's loop-edge checks on. The
// default has preemption off, and Postgres yields every 4096th
// CHECK_FOR_INTERRUPTS. Each sub-benchmark starts one server, outside the
// timer. Run them one at a time, for example:
//
//	go test -run '^$' -bench 'Start|CPUQuery' -benchtime 3x .
//	go test -run '^$' -bench 'Connect|Select1' .

// cpuQuery3e6 counts 3 million rows. The select-list form streams them.
// generate_series in FROM would materialize them into a tuplestore that
// spills to temporary files in the in-memory file system.
const cpuQuery3e6 = "SELECT count(*) FROM (SELECT generate_series(1, 3000000)) s"

var startModes = []struct {
	name  string
	start func(context.Context, pgmem.Options) (*pgmem.Server, error)
}{
	{"preempt=off", pgmem.Start},
	{"preempt=on", pgmem.StartPreempt},
}

// benchStart starts a server with startFn and closes it at cleanup.
func benchStart(b *testing.B, startFn func(context.Context, pgmem.Options) (*pgmem.Server, error)) *pgmem.Server {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaled(60*time.Second))
	defer cancel()
	s, err := startFn(ctx, pgmem.Options{})
	if notBuilt(err) {
		b.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Errorf("close: %v", err)
		}
	})
	return s
}

func benchConnect(b *testing.B, s *pgmem.Server) *pgx.Conn {
	b.Helper()
	c, err := pgx.ConnectConfig(context.Background(), connConfig(b, s))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// BenchmarkStart times Start until the server is ready. Close is untimed.
func BenchmarkStart(b *testing.B) {
	for _, m := range startModes {
		b.Run(m.name, func(b *testing.B) {
			for b.Loop() {
				ctx, cancel := context.WithTimeout(context.Background(), scaled(60*time.Second))
				s, err := m.start(ctx, pgmem.Options{})
				cancel()
				if notBuilt(err) {
					b.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
				}
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkConnect times a new connection, SELECT 1 and closing it.
func BenchmarkConnect(b *testing.B) {
	for _, m := range startModes {
		b.Run(m.name, func(b *testing.B) {
			s := benchStart(b, m.start)
			cfg := connConfig(b, s)
			ctx := context.Background()
			for b.Loop() {
				c, err := pgx.ConnectConfig(ctx, cfg)
				if err != nil {
					b.Fatal(err)
				}
				var one int
				if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
					b.Fatal(err)
				}
				if err := c.Close(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSelect1 times SELECT 1 on one open connection.
func BenchmarkSelect1(b *testing.B) {
	for _, m := range startModes {
		b.Run(m.name, func(b *testing.B) {
			c := benchConnect(b, benchStart(b, m.start))
			ctx := context.Background()
			var one int
			for b.Loop() {
				if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCPUQuery times a CPU-bound query that makes no host calls
// except the CHECK_FOR_INTERRUPTS yields.
func BenchmarkCPUQuery(b *testing.B) {
	for _, m := range startModes {
		b.Run(m.name, func(b *testing.B) {
			c := benchConnect(b, benchStart(b, m.start))
			ctx := context.Background()
			var n int64
			for b.Loop() {
				if err := c.QueryRow(ctx, cpuQuery3e6).Scan(&n); err != nil || n != 3000000 {
					b.Fatalf("count %d: %v", n, err)
				}
			}
		})
	}
}

// TestMemoryFootprint logs the test process's memory with 0, 1 and 20
// open connections. It runs only with PGMEM_MEASURE=1.
func TestMemoryFootprint(t *testing.T) {
	if os.Getenv("PGMEM_MEASURE") != "1" {
		t.Skip("set PGMEM_MEASURE=1 to measure memory")
	}
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	var conns []*pgx.Conn
	for _, n := range []int{0, 1, 20} {
		for len(conns) < n {
			c := connect(t, s)
			var one int
			if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		runtime.GC()
		time.Sleep(500 * time.Millisecond)
		logMemory(t, fmt.Sprintf("%d connections, %d guest processes", n, pgmem.Kernel(s).ProcCount()))
	}
}

// logMemory logs RSS, the Go heap and, where the OS has it, a measure that
// counts each shared page once. That is footprint on macOS and Pss on
// Linux. Linux RSS counts the shared segment once per mapping, and every
// guest process that attaches it maps it again.
func logMemory(t *testing.T, state string) {
	t.Helper()
	pid := fmt.Sprint(os.Getpid())
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss, err := osexec.Command("ps", "-o", "rss=", "-p", pid).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	t.Logf("%s: RSS %s KiB, Go HeapInuse %d MiB, Sys %d MiB",
		state, strings.TrimSpace(string(rss)), ms.HeapInuse>>20, ms.Sys>>20)
	switch runtime.GOOS {
	case "darwin":
		if _, err := osexec.LookPath("footprint"); err != nil {
			return
		}
		out, err := osexec.Command("footprint", pid).CombinedOutput()
		if err != nil {
			t.Logf("footprint: %v", err)
		}
		// Keep the summary, the column heads, the largest category, the
		// total and the peak. The largest category is "Untagged", which is
		// the Go heap, linear memories and compiled code.
		var keep []string
		for _, line := range strings.Split(string(out), "\n") {
			for _, key := range []string{"Footprint:", "Dirty", "Untagged", "TOTAL", "phys_footprint_peak"} {
				if strings.Contains(line, key) {
					keep = append(keep, line)
					break
				}
			}
		}
		t.Logf("footprint:\n%s", strings.Join(keep, "\n"))
	case "linux":
		data, err := os.ReadFile("/proc/self/smaps_rollup")
		if err != nil {
			t.Logf("smaps_rollup: %v", err)
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Rss:") || strings.HasPrefix(line, "Pss") {
				t.Logf("%s", line)
			}
		}
	}
}
