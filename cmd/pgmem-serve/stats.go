package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/feverenergy/pgmem/internal/rss"
)

const (
	samplePeriod   = 250 * time.Millisecond // RSS watchdog and pause sampling
	stallTick      = time.Millisecond
	stallThreshold = 20 * time.Millisecond
	pauseThreshold = 0.005 // seconds; pauses this long are logged as they happen
	worstStalls    = 15
)

// maxSampleFailures consecutive unreadable samples, about a second, trip
// the watchdog, so it never runs blind.
const maxSampleFailures = 4

// sampleRSS is the watchdog's sampler. Tests replace it.
var sampleRSS = rss.Resident

// watchdog samples RSS every samplePeriod, from its start. It closes over
// at the first sample past limit, or after maxSampleFailures consecutive
// samples fail.
type watchdog struct {
	limit  int64
	log    *log.Logger
	sample func() (int64, error)
	peak   atomic.Int64 // bytes
	cur    atomic.Int64 // the last sample
	reason error        // why over closed, written before it closes
	over   chan struct{}
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
}

// startWatchdog fails if RSS cannot be read, because running without the
// watchdog is never safe.
func startWatchdog(limit int64, logger *log.Logger) (*watchdog, error) {
	w := &watchdog{limit: limit, log: logger, sample: sampleRSS,
		over: make(chan struct{}), quit: make(chan struct{}), done: make(chan struct{})}
	if _, err := w.sample(); err != nil {
		return nil, fmt.Errorf("RSS watchdog: %w", err)
	}
	go w.loop()
	return w, nil
}

func (w *watchdog) loop() {
	defer close(w.done)
	tk := time.NewTicker(samplePeriod)
	defer tk.Stop()
	failures := 0
	for {
		v, err := w.sample()
		switch {
		case err != nil:
			failures++
			w.log.Printf("RSS watchdog: sample %d of %d failed: %v", failures, maxSampleFailures, err)
			if failures >= maxSampleFailures {
				w.trip(fmt.Errorf("RSS unreadable for %d samples in a row: %w", failures, err))
				return
			}
		default:
			failures = 0
			w.cur.Store(v)
			if v > w.peak.Load() {
				w.peak.Store(v)
			}
			if v > w.limit {
				w.trip(fmt.Errorf("RSS %d MiB passed -max-rss-mb %d", v>>20, w.limit>>20))
				return
			}
		}
		select {
		case <-tk.C:
		case <-w.quit:
			return
		}
	}
}

func (w *watchdog) trip(reason error) {
	w.log.Printf("%v: stopping the server", reason)
	w.reason = reason
	close(w.over)
}

// err describes the trip. Call it only once over is closed.
func (w *watchdog) err() error { return w.reason }

// stop ends the sampling. It waits for a sample in progress at most
// rss.Timeout plus a margin, so a hung sampler cannot block shutdown.
func (w *watchdog) stop() {
	w.once.Do(func() { close(w.quit) })
	select {
	case <-w.done:
	case <-time.After(rss.Timeout + time.Second):
		w.log.Printf("RSS watchdog: a sample is still running; not waiting for it")
	}
}

// stats writes measurements to a file. During the run it writes GC and
// other stop-the-world pauses of pauseThreshold or more, and scheduler
// stalls over stallThreshold, seen as a ticker goroutine woken late. Every
// second it writes RSS and heap. At the end it writes the pause
// histograms' totals and quantiles, the worst stalls and the peak RSS.
type stats struct {
	f     *os.File
	start time.Time
	wd    *watchdog
	quit  chan struct{}
	wg    sync.WaitGroup

	mu     sync.Mutex
	stalls []stall
	err    error // the first write error
}

type stall struct {
	at, dur time.Duration
}

var pauseMetrics = []string{
	"/sched/pauses/total/gc:seconds",
	"/sched/pauses/stopping/gc:seconds",
	"/sched/pauses/total/other:seconds",
	"/sched/pauses/stopping/other:seconds",
}

func startStats(path string, stwProbe time.Duration, wd *watchdog) (*stats, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	s := &stats{f: f, start: time.Now(), wd: wd, quit: make(chan struct{})}
	s.wg.Add(2)
	go s.stallLoop()
	go s.sampleLoop()
	if stwProbe > 0 {
		s.wg.Add(1)
		go s.stwLoop(stwProbe)
	}
	return s, nil
}

func (s *stats) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	_, err := fmt.Fprintf(s.f, "%s +%.3fs %s\n", now.Format("15:04:05.000"), now.Sub(s.start).Seconds(), fmt.Sprintf(format, args...))
	if err != nil && s.err == nil {
		s.err = err
	}
}

func (s *stats) stopped() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

// stallLoop sleeps stallTick at a time and logs wake-ups more than
// stallThreshold late. A late wake-up means the scheduler or a
// stop-the-world held it.
func (s *stats) stallLoop() {
	defer s.wg.Done()
	last := time.Now()
	for !s.stopped() {
		time.Sleep(stallTick)
		now := time.Now()
		if late := now.Sub(last) - stallTick; late > stallThreshold {
			s.logf("STALL %.1f ms (ended now)", ms(late))
			s.mu.Lock()
			s.stalls = append(s.stalls, stall{at: now.Sub(s.start), dur: late})
			s.mu.Unlock()
		}
		last = now
	}
}

// sampleLoop logs new pauses of pauseThreshold or more every samplePeriod,
// and RSS and heap figures every fourth period.
func (s *stats) sampleLoop() {
	defer s.wg.Done()
	samples := pauseSamples()
	prev := make([][]uint64, len(samples))
	var lastGC uint32
	tk := time.NewTicker(samplePeriod)
	defer tk.Stop()
	for i := 0; ; i++ {
		select {
		case <-tk.C:
		case <-s.quit:
			return
		}
		metrics.Read(samples)
		for j, smp := range samples {
			if smp.Value.Kind() != metrics.KindFloat64Histogram {
				continue
			}
			h := smp.Value.Float64Histogram()
			if prev[j] != nil {
				for b, c := range h.Counts {
					if d := c - prev[j][b]; d > 0 && h.Buckets[b] >= pauseThreshold {
						s.logf("PAUSE %s %d in [%.4g, %.4g) s", smp.Name, d, h.Buckets[b], h.Buckets[b+1])
					}
				}
			}
			prev[j] = slices.Clone(h.Counts)
		}
		if i%4 == 3 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			s.logf("RSS %d MiB heapInuse %d MiB sys %d MiB numGC %d (+%d)",
				s.wd.cur.Load()>>20, m.HeapInuse>>20, m.Sys>>20, m.NumGC, m.NumGC-lastGC)
			lastGC = m.NumGC
		}
	}
}

// stwLoop stops the world (ReadMemStats) every period and logs stops over
// 10 ms. A long one means some goroutine did not reach a safe point.
func (s *stats) stwLoop(period time.Duration) {
	defer s.wg.Done()
	var m runtime.MemStats
	for !s.stopped() {
		time.Sleep(period)
		t := time.Now()
		runtime.ReadMemStats(&m)
		if d := time.Since(t); d > 10*time.Millisecond {
			s.logf("STWPROBE %.1f ms (started at %s)", ms(d), t.Format("15:04:05.000"))
		}
	}
}

// finish stops the sampling and writes the summary.
func (s *stats) finish() error {
	close(s.quit)
	s.wg.Wait()
	samples := pauseSamples()
	metrics.Read(samples)
	for _, smp := range samples {
		if smp.Value.Kind() != metrics.KindFloat64Histogram {
			continue
		}
		h := smp.Value.Float64Histogram()
		var total uint64
		maxb := 0.0
		for b, c := range h.Counts {
			total += c
			if c > 0 {
				maxb = h.Buckets[b+1]
				if math.IsInf(maxb, 1) {
					maxb = h.Buckets[b]
				}
			}
		}
		s.logf("FINAL %s count %d max<%.4g s; p50 %.4g p99 %.4g p999 %.4g", smp.Name, total, maxb,
			quantile(h, 0.5), quantile(h, 0.99), quantile(h, 0.999))
	}
	s.mu.Lock()
	worst := slices.Clone(s.stalls)
	s.mu.Unlock()
	slices.SortFunc(worst, func(a, b stall) int { return int(b.dur - a.dur) })
	s.logf("FINAL %d stalls over %v", len(worst), stallThreshold)
	for _, w := range worst[:min(len(worst), worstStalls)] {
		s.logf("WORST STALL %.1f ms at +%.3fs", ms(w.dur), w.at.Seconds())
	}
	var ru syscall.Rusage
	maxrss := int64(0)
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		maxrss = int64(ru.Maxrss) // bytes on macOS, KiB elsewhere
		if runtime.GOOS != "darwin" {
			maxrss <<= 10
		}
	}
	peak := s.wd.peak.Load()
	if v, err := s.wd.sample(); err == nil { // a run shorter than samplePeriod has one sample, from before Start
		peak = max(peak, v)
	}
	s.logf("FINAL peak RSS %d MiB (sampled every %v); getrusage maxrss %d MiB", peak>>20, samplePeriod, maxrss>>20)
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	return errors.Join(err, s.f.Close())
}

func pauseSamples() []metrics.Sample {
	samples := make([]metrics.Sample, len(pauseMetrics))
	for i, n := range pauseMetrics {
		samples[i].Name = n
	}
	return samples
}

// quantile returns the upper bound of the bucket holding quantile q.
func quantile(h *metrics.Float64Histogram, q float64) float64 {
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(total)))
	var acc uint64
	for b, c := range h.Counts {
		acc += c
		if acc >= target {
			return h.Buckets[b+1]
		}
	}
	return math.Inf(1)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
