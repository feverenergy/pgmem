//go:build !darwin && !linux

package vmem

// pgmem maps memory with mmap and needs POSIX shared memory. On other
// systems the build fails here, with the error
// "undefined: pgmem_supports_only_macOS_and_Linux".
var _ = pgmem_supports_only_macOS_and_Linux
