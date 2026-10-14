package wasi_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// fakeSignals is a Signals whose pending state the test controls.
type fakeSignals struct {
	pending atomic.Bool
	wake    chan struct{}
}

func newFakeSignals() *fakeSignals { return &fakeSignals{wake: make(chan struct{}, 1)} }

func (f *fakeSignals) Deliverable() bool     { return f.pending.Load() }
func (f *fakeSignals) Wake() <-chan struct{} { return f.wake }
func (f *fakeSignals) send() {
	f.pending.Store(true)
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func TestWaitReturnsOnChannel(t *testing.T) {
	p := &wasi.Proc{}
	ch := make(chan struct{})
	go func() { time.Sleep(10 * time.Millisecond); close(ch) }()
	if err := p.Wait(context.Background(), time.Time{}, ch); err != wasiabi.ESUCCESS {
		t.Fatalf("errno %d", err)
	}
}

func TestWaitDeadline(t *testing.T) {
	p := &wasi.Proc{}
	start := time.Now()
	if err := p.Wait(context.Background(), start.Add(20*time.Millisecond)); err != wasiabi.ESUCCESS {
		t.Fatalf("errno %d", err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("returned before the deadline")
	}
}

func TestWaitInterruptedBySignal(t *testing.T) {
	sig := newFakeSignals()
	p := &wasi.Proc{Signals: sig}
	go func() { time.Sleep(10 * time.Millisecond); sig.send() }()
	if err := p.Wait(context.Background(), time.Time{}, make(chan struct{})); err != wasiabi.EINTR {
		t.Fatalf("errno %d, want EINTR", err)
	}
}

// A signal already pending when Wait starts must not be missed.
func TestWaitSeesSignalSentBefore(t *testing.T) {
	sig := newFakeSignals()
	sig.send()
	<-sig.wake // drained, so only Deliverable still says so
	p := &wasi.Proc{Signals: sig}
	done := make(chan wasiabi.Errno, 1)
	go func() { done <- p.Wait(context.Background(), time.Time{}, make(chan struct{})) }()
	select {
	case err := <-done:
		if err != wasiabi.EINTR {
			t.Fatalf("errno %d", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait missed a signal that was already pending")
	}
}

func TestWaitContextDone(t *testing.T) {
	p := &wasi.Proc{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx, time.Time{}, make(chan struct{})); err != wasiabi.EINTR {
		t.Fatalf("errno %d", err)
	}
}

// With more channels than the fixed select holds, any of them still wakes
// Wait.
func TestWaitManyChannels(t *testing.T) {
	p := &wasi.Proc{Signals: newFakeSignals()}
	chans := make([]<-chan struct{}, 5)
	for i := range chans {
		chans[i] = make(chan struct{})
	}
	last := make(chan struct{})
	chans[4] = last
	go func() { time.Sleep(10 * time.Millisecond); close(last) }()
	if err := p.Wait(context.Background(), time.Time{}, chans...); err != wasiabi.ESUCCESS {
		t.Fatalf("errno %d", err)
	}
	if err := p.Wait(context.Background(), time.Now().Add(10*time.Millisecond), chans[:4]...); err != wasiabi.ESUCCESS {
		t.Fatalf("deadline: errno %d", err)
	}
}

// Wait with more than three channels takes the reflect.Select path. It
// returns on a signal and on the end of its context. TestWaitManyChannels
// covers the channels themselves.
func TestWaitManyChannelsSignalAndContext(t *testing.T) {
	recv := make([]<-chan struct{}, 4)
	for i := range recv {
		recv[i] = make(chan struct{})
	}
	sig := newFakeSignals()
	p := &wasi.Proc{Signals: sig}
	go func() { time.Sleep(10 * time.Millisecond); sig.send() }()
	if err := p.Wait(context.Background(), time.Time{}, recv...); err != wasiabi.EINTR {
		t.Fatalf("signal: %d", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if err := (&wasi.Proc{}).Wait(ctx, time.Time{}, recv...); err != wasiabi.EINTR {
		t.Fatalf("context: %d", err)
	}
}
