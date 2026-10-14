//go:build unix

package pgtest

import (
	"errors"
	"os"
	"syscall"
)

// flock is syscall.Flock; tests replace it.
var flock = syscall.Flock

// lockFile opens path, creating it if needed, and takes an exclusive flock
// on it. It waits while another process, or another open file in this
// process, holds the lock. unlockFile releases it. The lock belongs to the
// open file, so the kernel releases it if the process dies.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, &os.PathError{Op: "flock", Path: path, Err: err}
	}
	return f, nil
}

// unlockFile releases the lock lockFile took and closes the file.
func unlockFile(f *os.File) error {
	err := flock(int(f.Fd()), syscall.LOCK_UN)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// lockUnsupported reports whether lockFile's err means that the file
// system does not support flock (NFS without a lock manager, some FUSE
// file systems), rather than that the lock file cannot be opened.
func lockUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
