package wasi

import (
	"context"
	"reflect"
	"time"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// Signals is what a process's pending signals look like to the WASI layer.
// The kernel implements it. nil means the process cannot be interrupted.
type Signals interface {
	// Deliverable reports whether a signal that is neither blocked nor
	// ignored is pending.
	Deliverable() bool
	// Wake returns a channel that receives, or is closed, when a signal is
	// sent to the process. Take it before checking Deliverable.
	Wake() <-chan struct{}
}

// Wait blocks until one of the channels fires, the deadline passes, a
// deliverable signal is pending, or ctx is done. A zero deadline means
// none. The last two cases return EINTR. Callers take every channel
// *before* checking the condition they wait for.
//
// Every blocking host call waits here, so a signal interrupts any of them.
// A signal that is sent but not deliverable, because it is blocked or
// ignored, does not end the wait. Wait takes Wake again and goes on
// waiting. So Wake must not stay ready for a signal that stays blocked, or
// Wait spins.
func (p *Proc) Wait(ctx context.Context, deadline time.Time, chans ...<-chan struct{}) wasiabi.Errno {
	if ctx.Err() != nil {
		return wasiabi.EINTR
	}
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return p.interrupted()
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	var cases []reflect.SelectCase // built once, for more than 3 channels
	for {
		// Take Wake before Deliverable, so a signal sent between the check
		// and the select still ends the wait.
		var wake <-chan struct{}
		if p.Signals != nil {
			wake = p.Signals.Wake()
			if p.Signals.Deliverable() {
				return wasiabi.EINTR
			}
		}
		var r waitResult
		if len(chans) <= 3 {
			r = selectFew(ctx, timeout, wake, chans)
		} else {
			if cases == nil {
				cases = selectCases(ctx, timeout, chans)
			}
			r = selectMany(cases, wake)
		}
		switch r {
		case waitFired, waitTimedOut:
			return wasiabi.ESUCCESS
		case waitCancelled:
			return wasiabi.EINTR
		}
		// waitSignalled. The signal is deliverable unless it is blocked or
		// ignored.
	}
}

// interrupted is Wait's answer when there is nothing to wait for. It is
// EINTR if a deliverable signal is pending, and success otherwise.
func (p *Proc) interrupted() wasiabi.Errno {
	if p.Signals != nil && p.Signals.Deliverable() {
		return wasiabi.EINTR
	}
	return wasiabi.ESUCCESS
}

type waitResult int

const (
	waitFired     waitResult = iota // one of the caller's channels
	waitTimedOut                    // the deadline passed
	waitCancelled                   // ctx is done
	waitSignalled                   // a signal was sent
)

// selectFew waits on up to three caller channels with a plain select. The
// missing ones are nil and never fire.
func selectFew(ctx context.Context, timeout <-chan time.Time, wake <-chan struct{}, chans []<-chan struct{}) waitResult {
	var c [3]<-chan struct{}
	copy(c[:], chans)
	select {
	case <-c[0]:
		return waitFired
	case <-c[1]:
		return waitFired
	case <-c[2]:
		return waitFired
	case <-timeout:
		return waitTimedOut
	case <-ctx.Done():
		return waitCancelled
	case <-wake:
		return waitSignalled
	}
}

// Fixed positions in the cases selectCases builds.
const (
	caseCtx = iota
	caseTimeout
	caseWake
	caseChans
)

// selectCases returns the select cases for ctx, timeout, a wake channel
// and chans, in that order. selectMany fills in the wake channel.
func selectCases(ctx context.Context, timeout <-chan time.Time, chans []<-chan struct{}) []reflect.SelectCase {
	cases := make([]reflect.SelectCase, caseChans+len(chans))
	for i := range cases {
		cases[i].Dir = reflect.SelectRecv
	}
	cases[caseCtx].Chan = reflect.ValueOf(ctx.Done())
	cases[caseTimeout].Chan = reflect.ValueOf(timeout)
	for i, ch := range chans {
		cases[caseChans+i].Chan = reflect.ValueOf(ch)
	}
	return cases
}

// selectMany waits on cases with wake in its slot. A nil channel's case
// never fires, as in a select statement.
func selectMany(cases []reflect.SelectCase, wake <-chan struct{}) waitResult {
	cases[caseWake].Chan = reflect.ValueOf(wake)
	switch i, _, _ := reflect.Select(cases); i {
	case caseCtx:
		return waitCancelled
	case caseTimeout:
		return waitTimedOut
	case caseWake:
		return waitSignalled
	}
	return waitFired
}
