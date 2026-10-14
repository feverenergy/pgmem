package kernel

import (
	"github.com/tetratelabs/wazero/experimental"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// SetMemoryAllocator makes every process k starts from now on get its
// memory from a instead of k's vmem allocator.
func SetMemoryAllocator(k *Kernel, a experimental.MemoryAllocator) {
	k.ctx = experimental.WithMemoryAllocator(k.ctx, a)
}

// MemoryStats reports on k's own vmem allocator, which holds the memories
// of every process k started. SetMemoryAllocator can replace it.
func MemoryStats(k *Kernel) vmem.Stats { return k.alloc.Stats() }

// ShmStats reports k's live shared-memory segments and attachments.
func ShmStats(k *Kernel) (segments, attachments int) { return k.shm.stats() }

// SemCount reports how many semaphores k holds.
func SemCount(k *Kernel) int { return k.sems.count() }

// FS returns k's file system.
func FS(k *Kernel) *memfs.FS { return k.fs }

// Listen makes a socket listening at the absolute path with backlog. It
// binds and registers it as sock_bind and sock_listen do for a guest, and
// returns it for the test to accept from and close.
func Listen(k *Kernel, path string, backlog int) (*wasi.Sock, wasiabi.Errno) {
	n, errno := k.fs.Open(k.fs.Root(), path, memfs.OpenFlags{Create: true, Excl: true})
	if errno != wasiabi.ESUCCESS {
		return nil, errno
	}
	s := wasi.NewSock()
	k.socks.add(n, s)
	if errno := s.Bind(path, func() { k.socks.remove(n, s) }); errno != wasiabi.ESUCCESS {
		return nil, errno
	}
	return s, s.Listen(backlog)
}
