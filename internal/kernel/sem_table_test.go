package kernel

import (
	"sync"
	"testing"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// waitTake blocks until it takes the semaphore.
func waitTake(t *testing.T, tb *semTable, k semKey) {
	for {
		ok, wake, e := tb.take(k)
		if e != wasiabi.ESUCCESS {
			t.Error(e)
			return
		}
		if ok {
			return
		}
		<-wake
	}
}

// Posters and waiters share one semaphore, and every post is taken exactly
// once.
func TestSemTablePostsAndTakes(t *testing.T) {
	tb := newSemTable()
	k := semKey{seg: 1, off: 16}
	tb.init(k, 0)
	const workers, n = 4, 500
	var wg sync.WaitGroup
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range n {
				waitTake(t, tb, k)
			}
		}()
		go func() {
			defer wg.Done()
			for range n {
				if e := tb.post(k); e != wasiabi.ESUCCESS {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
	if v, e := tb.value(k); e != wasiabi.ESUCCESS || v != 0 {
		t.Fatalf("value %d %v", v, e)
	}
}

func TestSemTableDrop(t *testing.T) {
	tb := newSemTable()
	shared, private := semKey{seg: 3, off: 0}, semKey{pid: 9, off: 4096}
	tb.init(shared, 0)
	tb.init(private, 0)
	_, wake, _ := tb.take(shared)
	tb.dropSegment(3)
	select {
	case <-wake:
	default:
		t.Fatal("waiter not woken when its segment went")
	}
	if _, _, e := tb.take(shared); e != wasiabi.EINVAL {
		t.Fatalf("take after drop: %v", e)
	}
	tb.dropProcess(9)
	if tb.count() != 0 {
		t.Fatalf("%d semaphores left", tb.count())
	}
	if e := tb.post(private); e != wasiabi.EINVAL {
		t.Fatalf("post after drop: %v", e)
	}
	tb.init(shared, semValueMax)
	if e := tb.post(shared); e != wasiabi.EOVERFLOW {
		t.Fatalf("post at max: %v", e)
	}
}
