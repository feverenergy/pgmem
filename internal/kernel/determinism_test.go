package kernel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"
)

func TestFixedClock(t *testing.T) {
	a, b := newFixedClock(), newFixedClock()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	for i := int64(1); i <= 3; i++ {
		if got, want := a(), epoch+i*1000; got != want {
			t.Fatalf("read %d: %d, want %d", i, got, want)
		}
	}
	// Each process counts its own reads.
	if got, want := b(), epoch+1000; got != want {
		t.Fatalf("second clock's first read: %d, want %d", got, want)
	}
}

func TestFixedRandom(t *testing.T) {
	var want []byte
	for i := range uint64(3) {
		sum := sha256.Sum256(binary.BigEndian.AppendUint64([]byte(fixedSeed), i))
		want = append(want, sum[:]...)
	}
	// Reads of any size see one continuous stream.
	r := newFixedRandom([]byte(fixedSeed))
	var got []byte
	for _, n := range []int{1, 30, 33, 0, 32} {
		p := make([]byte, n)
		if k, err := r.Read(p); k != n || err != nil {
			t.Fatalf("Read(%d) = %d, %v", n, k, err)
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream\n%x\nwant\n%x", got, want)
	}
}

// Each process of a deterministic kernel reads its own stream, seeded by
// fixedSeed and its pid.
func TestProcSeed(t *testing.T) {
	if got, want := procSeed(100), append([]byte(fixedSeed), 0, 0, 0, 100); !bytes.Equal(got, want) {
		t.Fatalf("procSeed(100) = %q, want %q", got, want)
	}
	a, b := make([]byte, 32), make([]byte, 32)
	newFixedRandom(procSeed(100)).Read(a)
	newFixedRandom(procSeed(101)).Read(b)
	if want := sha256.Sum256(binary.BigEndian.AppendUint64(procSeed(100), 0)); !bytes.Equal(a, want[:]) {
		t.Fatalf("pid 100's first block %x, want %x", a, want)
	}
	if bytes.Equal(a, b) {
		t.Fatal("pids 100 and 101 read the same stream")
	}
}
