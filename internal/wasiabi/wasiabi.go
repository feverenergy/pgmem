// Package wasiabi holds the WASI preview1 numbers pgmem's host side uses.
// wasi-libc's errno values are these same numbers.
package wasiabi

import "fmt"

// Errno is a WASI preview1 error number.
type Errno uint16

// Error implements error.
func (e Errno) Error() string { return fmt.Sprintf("wasi errno %d", uint16(e)) }

// Errno values, the subset pgmem returns.
const (
	ESUCCESS        Errno = 0
	E2BIG           Errno = 1
	EACCES          Errno = 2
	EADDRINUSE      Errno = 3
	EAFNOSUPPORT    Errno = 5
	EAGAIN          Errno = 6
	EBADF           Errno = 8
	ECHILD          Errno = 12
	ECONNREFUSED    Errno = 14
	ECONNRESET      Errno = 15
	EEXIST          Errno = 20
	EFAULT          Errno = 21
	EFBIG           Errno = 22
	EINTR           Errno = 27
	EINVAL          Errno = 28
	EIO             Errno = 29
	EISCONN         Errno = 30
	EISDIR          Errno = 31
	ELOOP           Errno = 32
	EMFILE          Errno = 33
	ENAMETOOLONG    Errno = 37
	ENOENT          Errno = 44
	ENOMEM          Errno = 48
	ENOSPC          Errno = 51
	ENOSYS          Errno = 52
	ENOTCONN        Errno = 53
	ENOTDIR         Errno = 54
	ENOTEMPTY       Errno = 55
	ENOTSOCK        Errno = 57
	ENOTSUP         Errno = 58
	ENOTTY          Errno = 59
	EOVERFLOW       Errno = 61
	EPERM           Errno = 63
	EPIPE           Errno = 64
	EPROTONOSUPPORT Errno = 66
	EPROTOTYPE      Errno = 67
	ERANGE          Errno = 68
	ESPIPE          Errno = 70
	ESRCH           Errno = 71
	ETIMEDOUT       Errno = 73
	EXDEV           Errno = 75
)

// Filetype values (filestat.filetype, fdstat.fs_filetype, dirent.d_type).
const (
	FiletypeUnknown         uint8 = 0
	FiletypeCharacterDevice uint8 = 2
	FiletypeDirectory       uint8 = 3
	FiletypeRegularFile     uint8 = 4
	FiletypeSocketStream    uint8 = 6
	FiletypeSymbolicLink    uint8 = 7
)

// Fdflags.
const (
	FdflagAppend   uint16 = 1
	FdflagDsync    uint16 = 2
	FdflagNonblock uint16 = 4
	FdflagRsync    uint16 = 8
	FdflagSync     uint16 = 16
)

// Oflags for path_open.
const (
	OflagCreat     uint16 = 1
	OflagDirectory uint16 = 2
	OflagExcl      uint16 = 4
	OflagTrunc     uint16 = 8
)

// LookupSymlinkFollow is the only lookupflags bit.
const LookupSymlinkFollow uint32 = 1

// Whence for fd_seek.
const (
	WhenceSet uint8 = 0
	WhenceCur uint8 = 1
	WhenceEnd uint8 = 2
)

// Fstflags for *_filestat_set_times.
const (
	FstflagAtim    uint16 = 1
	FstflagAtimNow uint16 = 2
	FstflagMtim    uint16 = 4
	FstflagMtimNow uint16 = 8
)

// Clock ids.
const (
	ClockRealtime  uint32 = 0
	ClockMonotonic uint32 = 1
)

// Event types and flags for poll_oneoff.
const (
	EventtypeClock   uint8  = 0
	EventtypeFdRead  uint8  = 1
	EventtypeFdWrite uint8  = 2
	EventHangup      uint16 = 1
	SubclockAbstime  uint16 = 1
)

// Riflags for sock_recv.
const (
	RiflagsRecvPeek    uint16 = 1
	RiflagsRecvWaitall uint16 = 2
)

// Sdflags for sock_shutdown.
const (
	SdflagsRd uint8 = 1
	SdflagsWr uint8 = 2
)

// Struct sizes in guest memory.
const (
	SizeIovec        = 8
	SizeFdstat       = 24
	SizeFilestat     = 64
	SizeDirent       = 24
	SizePrestat      = 8
	SizeSubscription = 48
	SizeEvent        = 32
)

// RightsAll is reported for every fd. pgmem does not enforce rights.
const RightsAll uint64 = 0x1FFFFFFF
