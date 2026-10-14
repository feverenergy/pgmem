//go:build race

package pgmem_test

// raceEnabled reports whether the race detector is on. It roughly doubles
// memory, so memory-bound tests skip under it.
const raceEnabled = true
