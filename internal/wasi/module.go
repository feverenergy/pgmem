package wasi

import (
	"context"
	"encoding/binary"
	"math"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// ModuleName is the import module name of WASI preview 1.
const ModuleName = "wasi_snapshot_preview1"

const (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
)

// hostFunc is one export of the module. fn reads its arguments from the
// call and returns the errno that becomes the function's only result.
type hostFunc struct {
	name   string
	params []api.ValueType
	fn     func(*call) wasiabi.Errno
}

func sig(params ...api.ValueType) []api.ValueType { return params }

// functions lists every preview1 export except proc_exit, which has no
// result.
var functions = []hostFunc{
	{"args_get", sig(i32, i32), argsGet},
	{"args_sizes_get", sig(i32, i32), argsSizesGet},
	{"environ_get", sig(i32, i32), environGet},
	{"environ_sizes_get", sig(i32, i32), environSizesGet},
	{"clock_res_get", sig(i32, i32), clockResGet},
	{"clock_time_get", sig(i32, i64, i32), clockTimeGet},
	{"fd_advise", sig(i32, i64, i64, i32), fdAdvise},
	{"fd_allocate", sig(i32, i64, i64), fdAllocate},
	{"fd_close", sig(i32), fdClose},
	{"fd_datasync", sig(i32), fdSync},
	{"fd_fdstat_get", sig(i32, i32), fdFdstatGet},
	{"fd_fdstat_set_flags", sig(i32, i32), fdFdstatSetFlags},
	{"fd_fdstat_set_rights", sig(i32, i64, i64), fdFdstatSetRights},
	{"fd_filestat_get", sig(i32, i32), fdFilestatGet},
	{"fd_filestat_set_size", sig(i32, i64), fdFilestatSetSize},
	{"fd_filestat_set_times", sig(i32, i64, i64, i32), fdFilestatSetTimes},
	{"fd_pread", sig(i32, i32, i32, i64, i32), fdPread},
	{"fd_prestat_get", sig(i32, i32), fdPrestatGet},
	{"fd_prestat_dir_name", sig(i32, i32, i32), fdPrestatDirName},
	{"fd_pwrite", sig(i32, i32, i32, i64, i32), fdPwrite},
	{"fd_read", sig(i32, i32, i32, i32), fdRead},
	{"fd_readdir", sig(i32, i32, i32, i64, i32), fdReaddir},
	{"fd_renumber", sig(i32, i32), fdRenumber},
	{"fd_seek", sig(i32, i64, i32, i32), fdSeek},
	{"fd_sync", sig(i32), fdSync},
	{"fd_tell", sig(i32, i32), fdTell},
	{"fd_write", sig(i32, i32, i32, i32), fdWrite},
	{"path_create_directory", sig(i32, i32, i32), pathCreateDirectory},
	{"path_filestat_get", sig(i32, i32, i32, i32, i32), pathFilestatGet},
	{"path_filestat_set_times", sig(i32, i32, i32, i32, i64, i64, i32), pathFilestatSetTimes},
	{"path_link", sig(i32, i32, i32, i32, i32, i32, i32), pathLink},
	{"path_open", sig(i32, i32, i32, i32, i32, i64, i64, i32, i32), pathOpen},
	{"path_readlink", sig(i32, i32, i32, i32, i32, i32), pathReadlink},
	{"path_remove_directory", sig(i32, i32, i32), pathRemoveDirectory},
	{"path_rename", sig(i32, i32, i32, i32, i32, i32), pathRename},
	{"path_symlink", sig(i32, i32, i32, i32, i32), pathSymlink},
	{"path_unlink_file", sig(i32, i32, i32), pathUnlinkFile},
	{"poll_oneoff", sig(i32, i32, i32, i32), pollOneoff},
	{"proc_raise", sig(i32), procRaise},
	{"random_get", sig(i32, i32), randomGet},
	{"sched_yield", sig(), schedYield},
	// compat implements accept itself over the pgm module, which can
	// report the peer address and set flags on the new fd.
	{"sock_accept", sig(i32, i32, i32), notSupported},
	{"sock_recv", sig(i32, i32, i32, i32, i32, i32), sockRecv},
	{"sock_send", sig(i32, i32, i32, i32, i32), sockSend},
	{"sock_shutdown", sig(i32, i32), sockShutdown},
}

// Instantiate adds the "wasi_snapshot_preview1" host module to r.
func Instantiate(ctx context.Context, r wazero.Runtime) error {
	b := r.NewHostModuleBuilder(ModuleName)
	for _, f := range functions {
		b.NewFunctionBuilder().
			WithGoModuleFunction(wrap(f.fn), f.params, sig(i32)).
			Export(f.name)
	}
	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(procExit), sig(i32), nil).
		Export("proc_exit")
	_, err := b.Instantiate(ctx)
	return err
}

// wrap adapts fn to wazero. It finds the Proc, or answers ENOSYS without
// one, and stores the errno in the result slot. ExitIfKilled kills a
// process whose context is done, on the way in and on the way out. Then
// the Proc's AfterCall hook runs, which delivers signals.
func wrap(fn func(*call) wasiabi.Errno) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		p := ProcFrom(ctx)
		if p == nil {
			stack[0] = uint64(wasiabi.ENOSYS)
			return
		}
		ExitIfKilled(ctx, mod)
		c := call{ctx: ctx, mod: mod, mem: mod.Memory(), p: p, stack: stack}
		stack[0] = uint64(fn(&c))
		ExitIfKilled(ctx, mod)
		if p.AfterCall != nil {
			p.AfterCall(ctx, mod)
		}
	}
}

// KillExitCode is the exit code of an instance ExitIfKilled ends. It is
// 128 plus SIGKILL, as a shell reports death by SIGKILL.
const KillExitCode = 128 + 9

// ExitIfKilled ends the calling instance if ctx is done, as proc_exit
// does. It closes the module and unwinds the call with a sys.ExitError
// carrying KillExitCode. A process's context is done once it has been
// killed, for example because the kernel is closing. Every host call
// checks it, on entry and after blocking, so a killed guest that retries
// EINTR cannot spin. ExitIfKilled does not stop a guest that makes no host
// calls at all.
func ExitIfKilled(ctx context.Context, mod api.Module) {
	if ctx.Err() == nil {
		return
	}
	_ = mod.CloseWithExitCode(ctx, KillExitCode)
	panic(sys.NewExitError(KillExitCode))
}

func notSupported(*call) wasiabi.Errno { return wasiabi.ENOTSUP }

// call is one host call. It holds the caller's module and memory, its
// Proc, and the raw argument slots.
type call struct {
	ctx   context.Context
	mod   api.Module
	mem   api.Memory
	p     *Proc
	stack []uint64
}

func (c *call) u32(i int) uint32 { return uint32(c.stack[i]) }
func (c *call) fd(i int) int32   { return int32(c.stack[i]) }
func (c *call) u64(i int) uint64 { return c.stack[i] }

// bytes returns the guest memory [ptr, ptr+n) itself. Writes to it land
// in the guest.
func (c *call) bytes(ptr, n uint32) ([]byte, wasiabi.Errno) {
	b, ok := c.mem.Read(ptr, n)
	if !ok {
		return nil, wasiabi.EFAULT
	}
	return b, wasiabi.ESUCCESS
}

func (c *call) putU32(ptr, v uint32) wasiabi.Errno {
	if !c.mem.WriteUint32Le(ptr, v) {
		return wasiabi.EFAULT
	}
	return wasiabi.ESUCCESS
}

func (c *call) putU64(ptr uint32, v uint64) wasiabi.Errno {
	if !c.mem.WriteUint64Le(ptr, v) {
		return wasiabi.EFAULT
	}
	return wasiabi.ESUCCESS
}

// put copies b into guest memory at ptr.
func (c *call) put(ptr uint32, b []byte) wasiabi.Errno {
	if !c.mem.Write(ptr, b) {
		return wasiabi.EFAULT
	}
	return wasiabi.ESUCCESS
}

// path reads the guest path whose pointer and length are arguments i and
// i+1. One longer than memfs.MaxPathLen fails with ENAMETOOLONG before it
// is copied, so a guest cannot make the host copy and split megabytes.
func (c *call) path(i int) (string, wasiabi.Errno) {
	if c.u32(i+1) > memfs.MaxPathLen {
		return "", wasiabi.ENAMETOOLONG
	}
	b, err := c.bytes(c.u32(i), c.u32(i+1))
	return string(b), err
}

// desc returns the description of the fd in argument i.
func (c *call) desc(i int) (*Desc, wasiabi.Errno) { return c.p.FDs.Get(c.fd(i)) }

// file returns the open file of the fd in argument i.
func (c *call) file(i int) (File, wasiabi.Errno) {
	d, err := c.desc(i)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	return d.file, wasiabi.ESUCCESS
}

// dir returns the directory node of the fd in argument i.
func (c *call) dir(i int) (*memfs.Node, wasiabi.Errno) {
	f, err := c.file(i)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	n := f.DirNode()
	if n == nil {
		return nil, wasiabi.ENOTDIR
	}
	return n, wasiabi.ESUCCESS
}

// dirPath returns the directory node and path of a path call whose dirfd is
// argument i and whose path is arguments j and j+1.
func (c *call) dirPath(i, j int) (*memfs.Node, string, wasiabi.Errno) {
	dir, err := c.dir(i)
	if err != wasiabi.ESUCCESS {
		return nil, "", err
	}
	path, err := c.path(j)
	return dir, path, err
}

// maxIovecs bounds an iovec array, like IOV_MAX. wasi-libc's IOV_MAX is
// 1024.
const maxIovecs = 1 << 16

// iovecs returns the guest buffers of the iovec or ciovec array at ptr.
func (c *call) iovecs(ptr, n uint32) ([][]byte, wasiabi.Errno) {
	if n > maxIovecs {
		return nil, wasiabi.EINVAL
	}
	raw, err := c.bytes(ptr, n*wasiabi.SizeIovec)
	if err != wasiabi.ESUCCESS {
		return nil, err
	}
	bufs := make([][]byte, n)
	total := 0
	for i := range bufs {
		e := raw[i*wasiabi.SizeIovec:]
		buf, err := c.bytes(le.Uint32(e), le.Uint32(e[4:]))
		if err != wasiabi.ESUCCESS {
			return nil, err
		}
		bufs[i] = buf
		total += len(buf)
	}
	if total > math.MaxInt32 {
		return nil, wasiabi.EINVAL // the count would overflow ssize_t, as readv says
	}
	return bufs, wasiabi.ESUCCESS
}

var le = binary.LittleEndian

// The encoders below write WASI structs in their guest layouts.

func encodeFdstat(filetype uint8, flags uint16, rights uint64) []byte {
	b := make([]byte, wasiabi.SizeFdstat)
	b[0] = filetype
	le.PutUint16(b[2:], flags)
	le.PutUint64(b[8:], rights)  // fs_rights_base
	le.PutUint64(b[16:], rights) // fs_rights_inheriting
	return b
}

func encodeFilestat(st memfs.Stat, filetype uint8) []byte {
	b := make([]byte, wasiabi.SizeFilestat)
	// dev (offset 0) stays 0, because there is one file system.
	le.PutUint64(b[8:], st.Ino)
	b[16] = filetype
	le.PutUint64(b[24:], st.Nlink)
	le.PutUint64(b[32:], uint64(st.Size))
	le.PutUint64(b[40:], uint64(st.Atim))
	le.PutUint64(b[48:], uint64(st.Mtim))
	le.PutUint64(b[56:], uint64(st.Ctim))
	return b
}

// encodePrestat describes a preopened directory (tag 0) named name.
func encodePrestat(name string) []byte {
	b := make([]byte, wasiabi.SizePrestat)
	le.PutUint32(b[4:], uint32(len(name)))
	return b
}

// encodeDirent returns a dirent header followed by the name. next is the
// cookie of the entry after it.
func encodeDirent(e memfs.Dirent, next uint64) []byte {
	b := make([]byte, wasiabi.SizeDirent+len(e.Name))
	le.PutUint64(b, next)
	le.PutUint64(b[8:], e.Ino)
	le.PutUint32(b[16:], uint32(len(e.Name)))
	b[20] = filetypeOf(e.Kind)
	copy(b[wasiabi.SizeDirent:], e.Name)
	return b
}
