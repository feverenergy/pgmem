package wasi

import "sync"

// Notifier is one object's wait queue. Broadcast wakes everyone waiting on
// a channel Changed returned. A pipe or socket owns one, so a state change
// wakes only the calls waiting on that object.
//
// Changed makes the channel on demand, and the next Broadcast closes it.
// So an object that changes state while nobody waits costs a lock and
// nothing more.
type Notifier struct {
	mu sync.Mutex
	ch chan struct{} // nil if nobody waits
}

// NewNotifier returns a notifier.
func NewNotifier() *Notifier { return &Notifier{} }

// Changed returns a channel that is closed at the next Broadcast. Take it
// before checking readiness, then wait on it, so no wake-up is lost.
// A nil Notifier returns a nil channel, which never fires.
func (n *Notifier) Changed() <-chan struct{} {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

// Broadcast wakes every waiter on a channel Changed returned. A nil
// Notifier does nothing.
func (n *Notifier) Broadcast() {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}

// Waitable is a File whose readiness can change while a call waits on it.
// Pipes and sockets are Waitable. Changed returns a channel that is closed
// at the file's next state change. Take it before checking readiness, then
// wait on it, so no change is lost.
type Waitable interface {
	Changed() <-chan struct{}
}
