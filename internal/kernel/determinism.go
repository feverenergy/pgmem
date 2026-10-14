package kernel

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// fixedEpoch is where a deterministic kernel's realtime clocks start.
var fixedEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()

// newFixedClock returns a realtime clock for one process of a
// deterministic kernel. It reads fixedEpoch plus 1µs for every read so
// far, starting at the first read.
func newFixedClock() func() int64 {
	var reads atomic.Int64
	return func() int64 { return fixedEpoch + reads.Add(1)*int64(time.Microsecond) }
}

// fixedSeed seeds a deterministic kernel's random streams.
const fixedSeed = "pgmem-mkcluster"

// fixedRandom is a deterministic byte stream, SHA-256 in counter mode.
// Block i is sha256(seed ‖ big-endian uint64 i), and the stream is the
// blocks in order. Reads never fail. It is safe for concurrent use, but
// the bytes each reader gets then depend on the order of the reads.
type fixedRandom struct {
	mu    sync.Mutex
	in    []byte // seed ‖ the counter of the next block
	ctr   []byte // the counter, which is the last 8 bytes of in
	next  uint64 // index of the next block to generate
	sum   [sha256.Size]byte
	block []byte // unread rest of the current block, within sum
}

func newFixedRandom(seed []byte) *fixedRandom {
	in := make([]byte, len(seed)+8)
	copy(in, seed)
	return &fixedRandom{in: in, ctr: in[len(seed):]}
}

// procSeed is the seed of process pid's stream in a deterministic kernel.
// It is fixedSeed ‖ big-endian uint32 pid. Each process reads its own
// stream, so the bytes it gets do not depend on when other processes read
// theirs. The kernel hands out pids in spawn order, and the guests make
// that order deterministic.
func procSeed(pid int32) []byte {
	return binary.BigEndian.AppendUint32([]byte(fixedSeed), uint32(pid))
}

func (r *fixedRandom) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		if len(r.block) == 0 {
			binary.BigEndian.PutUint64(r.ctr, r.next)
			r.sum = sha256.Sum256(r.in)
			r.block = r.sum[:]
			r.next++
		}
		c := copy(p[n:], r.block)
		r.block = r.block[c:]
		n += c
	}
	return n, nil
}
