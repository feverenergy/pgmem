package kernel

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// ABIVersion is the version of the pgm interface this kernel implements.
// A module that imports from pgm must export pgmem_abi_version returning
// it. The compat layer does so in postgres/compat/src/abi.c.
const ABIVersion = 5

const (
	pgmModule = "pgm"
	abiExport = "pgmem_abi_version"
)

// wnohang is WNOHANG in the options of pgm.wait.
const wnohang = 1

// maxFDMap bounds the fd map of one spawn.
const maxFDMap = 1024

// maxCwdBytes bounds the working directory passed to spawn. It is PATH_MAX
// (4096) less the NUL, so the child's PATH_MAX buffer always holds it.
const maxCwdBytes = 4095

// fdCloexec is FD_CLOEXEC in the flags of pgm.fd_getflags and fd_setflags.
const fdCloexec = 1

// maxArgBytes bounds the argv and environment of one process together,
// counting each string's NUL, like ARG_MAX. More is E2BIG. It keeps a guest
// from making the host copy and split megabytes.
const maxArgBytes = 2 << 20

// pgmFunc is one export of the pgm module. Every parameter and the result
// are i32. A negative result is -errno.
type pgmFunc struct {
	name   string
	params int
	fn     func(*pgmCall) int32
}

var pgmFuncs = []pgmFunc{
	{"getpid", 0, pgmGetpid},
	{"getppid", 0, pgmGetppid},
	{"pipe", 1, pgmPipe},
	{"spawn", 8, pgmSpawn},
	{"wait", 3, pgmWait},
	{"kill", 2, pgmKill},
	{"sig_register", 1, pgmSigRegister},
	{"setitimer", 4, pgmSetitimer},
	{"signal_exit", 1, pgmSignalExit},
	{"pause", 0, pgmPause},
	{"dup", 3, pgmDup},
	{"dup2", 2, pgmDup2},
	{"fd_getflags", 1, pgmFdGetflags},
	{"fd_setflags", 2, pgmFdSetflags},
	{"startup_cwd", 2, pgmStartupCwd},
	{"shm_get", 3, pgmShmGet},
	{"shm_attach", 2, pgmShmAttach},
	{"shm_detach", 1, pgmShmDetach},
	{"shm_stat", 2, pgmShmStat},
	{"shm_rmid", 1, pgmShmRmid},
	{"sem_init", 2, pgmSemInit},
	{"sem_destroy", 1, pgmSemDestroy},
	{"sem_post", 1, pgmSemPost},
	{"sem_wait", 3, pgmSemWait},
	{"sem_getvalue", 1, pgmSemGetvalue},
	{"sock_open", 1, pgmSockOpen},
	{"sock_bind", 3, pgmSockBind},
	{"sock_listen", 2, pgmSockListen},
	{"sock_connect", 3, pgmSockConnect},
	{"sock_accept", 2, pgmSockAccept},
	{"sock_name", 4, pgmSockName},
	{"yield", 0, pgmYield},
}

// instantiatePGM adds the pgm host module to the kernel's runtime.
func (k *Kernel) instantiatePGM(ctx context.Context) error {
	b := k.rt.NewHostModuleBuilder(pgmModule)
	for _, f := range pgmFuncs {
		params := make([]api.ValueType, f.params)
		for i := range params {
			params[i] = api.ValueTypeI32
		}
		b.NewFunctionBuilder().
			WithGoModuleFunction(k.hostFunc(f.fn), params, []api.ValueType{api.ValueTypeI32}).
			Export(f.name)
	}
	_, err := b.Instantiate(ctx)
	return err
}

// hostFunc adapts fn to wazero. A caller that is not a kernel process gets
// -ENOSYS. A killed process ends on the way in or out, as at every WASI
// call (wasi.ExitIfKilled). Signals go to the guest on the way out, as
// after every WASI call (wasi.Proc.AfterCall).
func (k *Kernel) hostFunc(fn func(*pgmCall) int32) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		p := procFrom(ctx)
		if p == nil {
			stack[0] = api.EncodeI32(fail(wasiabi.ENOSYS))
			return
		}
		wasi.ExitIfKilled(ctx, mod)
		c := pgmCall{ctx: ctx, k: k, p: p, mod: mod, mem: mod.Memory(), stack: stack}
		stack[0] = api.EncodeI32(fn(&c))
		wasi.ExitIfKilled(ctx, mod)
		if hook := p.wasi.AfterCall; hook != nil {
			hook(ctx, mod)
		}
	}
}

// pgmCall is one pgm host call.
type pgmCall struct {
	ctx   context.Context
	k     *Kernel
	p     *proc
	mod   api.Module
	mem   api.Memory
	stack []uint64
}

func (c *pgmCall) i32(i int) int32  { return api.DecodeI32(c.stack[i]) }
func (c *pgmCall) u32(i int) uint32 { return api.DecodeU32(c.stack[i]) }

func fail(e wasiabi.Errno) int32 { return -int32(e) }

// strings reads the NUL-separated, NUL-terminated strings whose pointer
// and length are arguments i and i+1.
func (c *pgmCall) strings(i int) ([]string, wasiabi.Errno) {
	n := c.u32(i + 1)
	if n == 0 {
		return nil, wasiabi.ESUCCESS
	}
	b, ok := c.mem.Read(c.u32(i), n)
	if !ok {
		return nil, wasiabi.EFAULT
	}
	if b[n-1] != 0 {
		return nil, wasiabi.EINVAL
	}
	return strings.Split(string(b[:n-1]), "\x00"), wasiabi.ESUCCESS
}

// pgmYield implements yield() -> 0, which does nothing. Compat calls it
// every 4096th CHECK_FOR_INTERRUPTS (PGMEM_CFI_POLL), so a computing guest
// reaches the host often enough to be killed and to let Go stop the world.
// As on every pgm call, a killed process ends on the way out, and pending
// signals go to the guest.
func pgmYield(*pgmCall) int32 { return 0 }

// pgmGetpid implements getpid() -> pid.
func pgmGetpid(c *pgmCall) int32 { return c.p.pid }

// pgmGetppid implements getppid() -> pid.
func pgmGetppid(c *pgmCall) int32 {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	return c.p.ppid
}

// pgmPipe implements pipe(fds_ptr) -> 0 | -errno. It installs a new
// pipe's read end, then its write end, and stores both fds at fds_ptr. It
// fails with EMFILE if there is no room for both.
func pgmPipe(c *pgmCall) int32 {
	ptr := c.u32(0)
	if _, ok := c.mem.Read(ptr, 8); !ok {
		return fail(wasiabi.EFAULT)
	}
	r, w := wasi.NewPipe()
	fds := c.p.wasi.FDs
	rfd, errno := fds.Install(wasi.NewDesc(r, 0))
	if errno != wasiabi.ESUCCESS {
		w.Close()
		return fail(errno)
	}
	wfd, errno := fds.Install(wasi.NewDesc(w, 0))
	if errno != wasiabi.ESUCCESS {
		fds.Close(rfd)
		return fail(errno)
	}
	c.mem.WriteUint32Le(ptr, uint32(rfd))
	c.mem.WriteUint32Le(ptr+4, uint32(wfd))
	return 0
}

// pgmSpawn implements spawn(argv_ptr, argv_len, envp_ptr, envp_len,
// cwd_ptr, cwd_len, fdmap_ptr, fdmap_count) -> pid | -errno. argv_len and
// envp_len together may not exceed maxArgBytes (E2BIG).
//
// The child gets fd 3, the root preopen. It then gets every other fd of
// the parent that is not close-on-exec, at the same number. Each mapped fd
// then becomes a dup of the parent's, over whatever the child inherited
// there.
//
// cwd is cwd_len bytes with no NUL, or empty for none. The child starts
// there, and its compat layer reads it with startup_cwd. The child starts
// with the parent's blocked signals still blocked, as a forked and exec'd
// child does. Postgres blocks all of them around fork.
func pgmSpawn(c *pgmCall) int32 {
	if uint64(c.u32(1))+uint64(c.u32(3)) > maxArgBytes {
		return fail(wasiabi.E2BIG)
	}
	argv, errno := c.strings(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	env, errno := c.strings(2)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	if len(argv) == 0 {
		return fail(wasiabi.EINVAL)
	}
	cwdLen := c.u32(5)
	if cwdLen > maxCwdBytes {
		return fail(wasiabi.ENAMETOOLONG)
	}
	rawCwd, ok := c.mem.Read(c.u32(4), cwdLen)
	if !ok {
		return fail(wasiabi.EFAULT)
	}
	cwd := string(rawCwd)
	if strings.IndexByte(cwd, 0) >= 0 {
		return fail(wasiabi.EINVAL)
	}
	prog, ok := c.k.program(argv[0])
	if !ok {
		return fail(wasiabi.ENOENT)
	}
	// A working directory that is gone fails the spawn. A child started at
	// "/" would fail later on relative paths, and the cause would be hard
	// to see.
	if cwd != "" {
		n, errno := c.k.fs.Lookup(c.k.fs.Root(), cwd, true)
		if errno != wasiabi.ESUCCESS {
			return fail(errno)
		}
		if c.k.fs.Stat(n).Kind != memfs.KindDir {
			return fail(wasiabi.ENOTDIR)
		}
	}

	count := c.i32(7)
	if count < 0 || count > maxFDMap {
		return fail(wasiabi.EINVAL)
	}
	raw, ok := c.mem.Read(c.u32(6), uint32(count)*8)
	if !ok {
		return fail(wasiabi.EFAULT)
	}
	type pair struct{ child, parent int32 }
	pairs := make([]pair, count)
	for i := range pairs {
		pairs[i] = pair{
			child:  int32(binary.LittleEndian.Uint32(raw[8*i:])),
			parent: int32(binary.LittleEndian.Uint32(raw[8*i+4:])),
		}
		switch {
		case pairs[i].child == 3:
			return fail(wasiabi.EINVAL) // always the root preopen
		case pairs[i].child < 0 || pairs[i].child >= wasi.MaxFiles:
			return fail(wasiabi.EBADF)
		}
	}

	parent := c.p.wasi.FDs
	fds := wasi.NewChildFDs(c.k.fs)
	parent.InheritInto(fds)
	for _, m := range pairs {
		d, errno := parent.Dup(m.parent)
		if errno != wasiabi.ESUCCESS {
			fds.CloseAll()
			return fail(errno)
		}
		fds.InstallAt(m.child, d)
	}

	child, errno, _ := c.k.start(c.p.pid, false, prog, argv, env, cwd, c.p.sigs.blockedMask(), fds) // EAGAIN closed, ENOMEM
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return child.pid
}

// pgmDup implements dup(fd, min, cloexec) -> newfd | -errno. It returns
// the lowest free fd >= min for fd's description. The new fd is
// close-on-exec if cloexec is non-zero.
func pgmDup(c *pgmCall) int32 {
	fd, errno := c.p.wasi.FDs.DupFrom(c.i32(0), c.i32(1), c.i32(2) != 0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return fd
}

// pgmDup2 implements dup2(fd, newfd) -> newfd | -errno. newfd then refers
// to fd's description and is not close-on-exec. It closes what newfd held.
// dup2(fd, fd) only checks that fd is open.
func pgmDup2(c *pgmCall) int32 {
	fd, newfd := c.i32(0), c.i32(1)
	if errno := c.p.wasi.FDs.Dup2(fd, newfd); errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return newfd
}

// pgmFdGetflags implements fd_getflags(fd) -> flags | -errno. The flags
// are FD_CLOEXEC (1) or 0.
func pgmFdGetflags(c *pgmCall) int32 {
	on, errno := c.p.wasi.FDs.Cloexec(c.i32(0))
	switch {
	case errno != wasiabi.ESUCCESS:
		return fail(errno)
	case on:
		return fdCloexec
	}
	return 0
}

// pgmFdSetflags implements fd_setflags(fd, flags) -> 0 | -errno. It sets
// fd's close-on-exec flag from FD_CLOEXEC in flags. Like Linux, it ignores
// other bits.
func pgmFdSetflags(c *pgmCall) int32 {
	if errno := c.p.wasi.FDs.SetCloexec(c.i32(0), c.i32(1)&fdCloexec != 0); errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return 0
}

// pgmStartupCwd implements startup_cwd(buf, len) -> n | -errno. It copies
// the working directory the parent passed to spawn, with no NUL, and
// returns its length. It returns 0 if there is none, as for a process
// started from Go. It fails with ERANGE if the directory does not fit in
// len.
func pgmStartupCwd(c *pgmCall) int32 {
	cwd := c.p.cwd
	if len(cwd) == 0 {
		return 0
	}
	if uint32(len(cwd)) > c.u32(1) {
		return fail(wasiabi.ERANGE)
	}
	if !c.mem.Write(c.u32(0), []byte(cwd)) {
		return fail(wasiabi.EFAULT)
	}
	return int32(len(cwd))
}

// pgmWait implements wait(pid, status_ptr, options) -> pid | 0 | -errno.
// pid > 0 waits for that child. Any other pid waits for any child, because
// each process is its own group. Without WNOHANG it blocks until a child
// exits. A deliverable signal interrupts it with -EINTR, unless a child
// has exited by then. A zombie wins over EINTR, so the SIGCHLD of the very
// child waited for does not fail the wait.
func pgmWait(c *pgmCall) int32 {
	pid, ptr, options := c.i32(0), c.u32(1), c.i32(2)
	if _, ok := c.mem.Read(ptr, 4); !ok {
		return fail(wasiabi.EFAULT)
	}
	for {
		// Get the channel before the check, so an exit between the two is
		// not lost.
		exited := c.k.exited.Changed()
		r, done := c.reap(pid, ptr, options)
		if done {
			return r
		}
		if errno := c.p.wasi.Wait(c.ctx, time.Time{}, exited); errno != wasiabi.ESUCCESS {
			if errno == wasiabi.EINTR && c.ctx.Err() == nil {
				if r, done := c.reap(pid, ptr, options|wnohang); done && r > 0 {
					return r
				}
			}
			return fail(errno)
		}
	}
}

// reap is one round of wait. It reaps a matching zombie, or reports that
// there is nothing to wait for (ECHILD, or 0 with WNOHANG). Otherwise it
// returns done false, and the caller must wait.
func (c *pgmCall) reap(pid int32, ptr uint32, options int32) (int32, bool) {
	k := c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	var zombie *proc
	found := false
	for _, q := range k.procs {
		if q.ppid != c.p.pid || (pid > 0 && q.pid != pid) {
			continue
		}
		found = true
		if q.exited && (zombie == nil || q.pid < zombie.pid) {
			zombie = q
		}
	}
	switch {
	case zombie != nil:
		delete(k.procs, zombie.pid)
		c.mem.WriteUint32Le(ptr, uint32(zombie.status))
		return zombie.pid, true
	case !found:
		return fail(wasiabi.ECHILD), true
	case options&wnohang != 0:
		return 0, true
	}
	return 0, false
}

// pgmKill implements kill(pid, sig) -> 0 | -errno. sig 0 checks that pid
// exists. A negative pid names a process group. Every process leads its
// own, so it means -pid. Pid 0 means the caller. Pid 1, the Go caller,
// always exists but takes no signals (EPERM). A zombie takes any signal
// and ignores it. A signal to the caller itself reaches it as this call
// returns.
func pgmKill(c *pgmCall) int32 {
	pid, sig := c.i32(0), c.i32(1)
	if sig < 0 || sig > maxSignal {
		return fail(wasiabi.EINVAL)
	}
	switch {
	case pid == 0:
		pid = c.p.pid
	case pid == math.MinInt32:
		return fail(wasiabi.ESRCH)
	case pid < 0:
		pid = -pid
	}
	if pid == initPID {
		if sig != 0 {
			return fail(wasiabi.EPERM)
		}
		return 0
	}
	q, _ := c.k.lookup(pid)
	if q == nil {
		return fail(wasiabi.ESRCH)
	}
	if sig != 0 {
		q.signal(int(sig))
	}
	return 0
}

// pgmSigRegister implements sig_register(addr) -> 0 | -errno. It attaches
// the guest's signal block, a struct pgmem_sigstate { uint64_t pending,
// blocked, ignored; } at an 8-byte aligned addr. Signals sent before the
// call become pending.
func pgmSigRegister(c *pgmCall) int32 {
	addr := c.u32(0)
	if addr%8 != 0 {
		return fail(wasiabi.EINVAL)
	}
	b, ok := sigBlockAt(c.mem, addr)
	if !ok {
		return fail(wasiabi.EFAULT)
	}
	c.p.sigs.register(b)
	return 0
}

// pgmSetitimer implements setitimer(value_usec_lo, value_usec_hi,
// interval_usec_lo, interval_usec_hi) -> 0. It arms the real-time timer to
// send SIGALRM after value and then every interval. An interval of 0 fires
// once. A zero value disarms the timer. A deterministic kernel never arms
// it, because when it fired would depend on host speed.
func pgmSetitimer(c *pgmCall) int32 {
	if c.k.deterministic {
		return 0
	}
	value := uint64(c.u32(0)) | uint64(c.u32(1))<<32
	interval := uint64(c.u32(2)) | uint64(c.u32(3))<<32
	c.p.sigs.setTimer(usecDuration(value), usecDuration(interval))
	return 0
}

// pgmSignalExit implements signal_exit(sig), which never returns. The
// process ends, killed by sig, like proc_exit with the shell's code
// 128+sig.
func pgmSignalExit(c *pgmCall) int32 {
	sig := c.i32(0)
	if sig < 1 || sig > maxSignal {
		return fail(wasiabi.EINVAL)
	}
	c.p.exitSig.Store(sig)
	code := uint32(128 + sig)
	_ = c.mod.CloseWithExitCode(c.ctx, code)
	panic(sys.NewExitError(code))
}

// pgmPause implements pause() -> -EINTR. It waits until a deliverable
// signal is pending.
func pgmPause(c *pgmCall) int32 {
	for {
		if errno := c.p.wasi.Wait(c.ctx, time.Time{}); errno != wasiabi.ESUCCESS {
			return fail(errno)
		}
	}
}

// memBytes is the whole of a process's memory, as the slice vmem knows it
// by.
func memBytes(m api.Memory) []byte {
	b, _ := m.Read(0, m.Size())
	return b
}

// pgmShmGet implements shm_get(key, size, flags) -> id | -errno, which is
// shmget.
func pgmShmGet(c *pgmCall) int32 {
	id, errno := c.k.shm.get(c.p.pid, c.i32(0), c.u32(1), c.i32(2))
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return id
}

// pgmShmAttach implements shm_attach(id, addr) -> 0 | -errno. It maps
// segment id at addr. addr is a nonzero multiple of 16 KiB. The whole
// mapping, shm_stat's mapped bytes, must lie inside the memory and clear
// of other attachments.
func pgmShmAttach(c *pgmCall) int32 {
	return fail(c.k.shm.attach(c.p.pid, memBytes(c.mem), c.i32(0), c.u32(1)))
}

// pgmShmDetach implements shm_detach(addr) -> 0 | -errno. It undoes the
// attachment at addr.
func pgmShmDetach(c *pgmCall) int32 {
	return fail(c.k.shm.detach(c.p.pid, memBytes(c.mem), c.u32(0)))
}

// pgmShmStat implements shm_stat(id, buf) -> 0 | -errno. It writes struct
// pgmem_shm_stat { int32_t key; uint32_t size, mapped, nattch; int32_t
// cpid, lpid; } at buf.
func pgmShmStat(c *pgmCall) int32 {
	st, errno := c.k.shm.stat(c.i32(0))
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	var b [24]byte
	binary.LittleEndian.PutUint32(b[0:], uint32(st.key))
	binary.LittleEndian.PutUint32(b[4:], st.size)
	binary.LittleEndian.PutUint32(b[8:], st.mapped)
	binary.LittleEndian.PutUint32(b[12:], st.nattch)
	binary.LittleEndian.PutUint32(b[16:], uint32(st.cpid))
	binary.LittleEndian.PutUint32(b[20:], uint32(st.lpid))
	if !c.mem.Write(c.u32(1), b[:]) {
		return fail(wasiabi.EFAULT)
	}
	return 0
}

// pgmShmRmid implements shm_rmid(id) -> 0 | -errno, which is IPC_RMID.
func pgmShmRmid(c *pgmCall) int32 {
	return fail(c.k.shm.rmid(c.i32(0)))
}

// semKey resolves the sem_t at argument i. The key is its segment and
// offset if the process has a segment mapped there. Otherwise it is the
// sem_t's address in the process.
func (c *pgmCall) semKey(i int) (semKey, wasiabi.Errno) {
	addr := c.u32(i)
	if addr%4 != 0 {
		return semKey{}, wasiabi.EINVAL
	}
	if _, ok := c.mem.Read(addr, semSize); !ok {
		return semKey{}, wasiabi.EFAULT
	}
	if seg, off, ok := c.k.shm.locate(c.p.pid, addr, semSize); ok {
		return semKey{seg: seg, off: off}, wasiabi.ESUCCESS
	}
	return semKey{pid: c.p.pid, off: addr}, wasiabi.ESUCCESS
}

// pgmSemInit implements sem_init(addr, value) -> 0 | -errno. It creates
// or resets the semaphore.
func pgmSemInit(c *pgmCall) int32 {
	k, errno := c.semKey(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	if c.u32(1) > semValueMax {
		return fail(wasiabi.EINVAL)
	}
	c.k.sems.init(k, c.u32(1))
	return 0
}

// pgmSemDestroy implements sem_destroy(addr) -> 0 | -errno.
func pgmSemDestroy(c *pgmCall) int32 {
	k, errno := c.semKey(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return fail(c.k.sems.destroy(k))
}

// pgmSemPost implements sem_post(addr) -> 0 | -errno.
func pgmSemPost(c *pgmCall) int32 {
	k, errno := c.semKey(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return fail(c.k.sems.post(k))
}

// pgmSemWait implements sem_wait(addr, mode, ts) -> 0 | -errno. Mode 0
// blocks. Mode 1 tries and fails with EAGAIN. Mode 2 blocks until the
// absolute CLOCK_REALTIME time in the struct timespec at ts, then fails
// with ETIMEDOUT. The timespec is int64 seconds, then int32 nanoseconds. A
// signal interrupts the wait with EINTR.
func pgmSemWait(c *pgmCall) int32 {
	k, errno := c.semKey(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	mode := c.i32(1)
	var deadline time.Time
	switch mode {
	case semBlock, semTry:
	case semTimed:
		b, ok := c.mem.Read(c.u32(2), 12)
		if !ok {
			return fail(wasiabi.EFAULT)
		}
		sec := int64(binary.LittleEndian.Uint64(b))
		nsec := int32(binary.LittleEndian.Uint32(b[8:]))
		if nsec < 0 || nsec >= 1e9 {
			return fail(wasiabi.EINVAL)
		}
		deadline = time.Unix(sec, int64(nsec))
	default:
		return fail(wasiabi.EINVAL)
	}
	for {
		ok, wake, errno := c.k.sems.take(k)
		switch {
		case errno != wasiabi.ESUCCESS:
			return fail(errno)
		case ok:
			return 0
		case mode == semTry:
			return fail(wasiabi.EAGAIN)
		case !deadline.IsZero() && !time.Now().Before(deadline):
			return fail(wasiabi.ETIMEDOUT)
		}
		if errno := c.p.wasi.Wait(c.ctx, deadline, wake); errno != wasiabi.ESUCCESS {
			return fail(errno)
		}
	}
}

// pgmSemGetvalue implements sem_getvalue(addr) -> value | -errno.
func pgmSemGetvalue(c *pgmCall) int32 {
	k, errno := c.semKey(0)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	v, errno := c.k.sems.value(k)
	if errno != wasiabi.ESUCCESS {
		return fail(errno)
	}
	return int32(v)
}
