package kernel

// Status is a wait status in the encoding compat/include/sys/wait.h
// expects. The low 7 bits are the terminating signal, or 0 for a normal
// exit. Bits 8-15 are the exit code.
type Status int32

// ExitStatus is the status of a process that exited with code. Only the low
// 8 bits of code survive, as on POSIX.
func ExitStatus(code uint32) Status { return Status(code&0xff) << 8 }

// TrapStatus is the status of a process that trapped. It reads as death
// by SIGABRT.
const TrapStatus Status = 6

// Exited reports whether the process exited normally.
func (s Status) Exited() bool { return s&0x7f == 0 }

// ExitCode returns the exit code, or -1 if the process did not exit
// normally.
func (s Status) ExitCode() int {
	if !s.Exited() {
		return -1
	}
	return int(s>>8) & 0xff
}

// Signaled reports whether a signal terminated the process.
func (s Status) Signaled() bool {
	sig := s & 0x7f
	return sig != 0 && sig != 0x7f
}

// Signal returns the terminating signal, or 0 if there was none.
func (s Status) Signal() int {
	if !s.Signaled() {
		return 0
	}
	return int(s & 0x7f)
}

// KilledStatus is the status of a process the kernel killed. It reads as
// death by SIGKILL. Kernel.Close kills every process still running.
const KilledStatus Status = 9
