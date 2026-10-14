//go:build race

package cluster_test

// raceEnabled reports whether the race detector is on. It slows the
// wazero interpreter, which deterministic kernels run, several times over.
const raceEnabled = true
