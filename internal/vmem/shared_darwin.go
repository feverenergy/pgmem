package vmem

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var shmSeq atomic.Uint64

// newSharedFD returns a descriptor for a size-byte anonymous shared
// object. That is a POSIX shared-memory object, unlinked at once. Names
// stay under macOS's 31-byte limit.
//
// x/sys has no shm_open or shm_unlink wrapper on darwin, so newSharedFD
// calls both by syscall number, through libSystem's syscall(2). Apple
// supports only the libSystem functions. It does not promise that syscall
// numbers stay the same, and it has deprecated syscall(2). Both numbers
// have been unchanged for many releases, but a future macOS could change
// or drop them, and then pgmem could not create shared memory. The fix
// would be to call libSystem's shm_open and shm_unlink, as x/sys does
// for other calls.
func newSharedFD(size int64) (int, error) {
	for {
		name, err := unix.BytePtrFromString(fmt.Sprintf("/pgmem.%d.%d", os.Getpid(), shmSeq.Add(1)))
		if err != nil {
			return -1, err
		}
		// shm_open has no O_CLOEXEC. Hold ForkLock until the descriptor
		// is close-on-exec, so a concurrent os/exec cannot inherit it.
		syscall.ForkLock.RLock()
		// A raw syscall; see the stability note above.
		r, _, e := syscall.Syscall(unix.SYS_SHM_OPEN, uintptr(unsafe.Pointer(name)),
			uintptr(unix.O_RDWR|unix.O_CREAT|unix.O_EXCL), 0o600)
		if e != 0 {
			syscall.ForkLock.RUnlock()
			if e == unix.EEXIST {
				continue
			}
			return -1, e
		}
		fd := int(r)
		unix.CloseOnExec(fd)
		syscall.ForkLock.RUnlock()
		// A raw syscall too.
		if _, _, e := syscall.Syscall(unix.SYS_SHM_UNLINK, uintptr(unsafe.Pointer(name)), 0, 0); e != 0 {
			unix.Close(fd)
			return -1, e
		}
		if err := unix.Ftruncate(fd, size); err != nil {
			unix.Close(fd)
			return -1, err
		}
		return fd, nil
	}
}
