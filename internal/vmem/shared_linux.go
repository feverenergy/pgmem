package vmem

import "golang.org/x/sys/unix"

// newSharedFD returns a descriptor for a size-byte anonymous shared object.
func newSharedFD(size int64) (int, error) {
	fd, err := unix.MemfdCreate("pgmem-shm", unix.MFD_CLOEXEC)
	if err != nil {
		return -1, err
	}
	if err := unix.Ftruncate(fd, size); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
