// Package kernel runs pgmem processes. Each process is a wazero instance
// running in its own goroutine. All of them share one runtime, one memfs
// and the two host modules every instance imports: pgmem's WASI and "pgm".
// The compat layer spawns, waits for and signals processes through "pgm".
package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/sys"

	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/vmem"
	"github.com/feverenergy/pgmem/internal/wasi"
	"github.com/feverenergy/pgmem/internal/wasiabi"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// Pid 1 is the Go caller. Processes count up from firstPID.
const (
	initPID  = 1
	firstPID = 100
)

// execFailed is the exit code of a process that could not start. A shell
// reports a command it cannot run with the same code.
const execFailed = 127

// Kernel runs pgmem processes. They are wazero instances that share one
// memfs and talk through pgmem's WASI and the pgm host module.
type Kernel struct {
	rt      wazero.Runtime
	fs      *memfs.FS
	alloc   *vmem.Allocator
	shm     *shmTable
	sems    *semTable
	socks   *sockRegistry
	preempt bool // wazero closes a module when its call's context is done

	// deterministic is Config.Deterministic. When it is set, each process
	// gets a fixed clock and its own fixed random stream.
	deterministic bool

	// ctx carries the allocator and is the parent of every process's
	// context. cancel interrupts every blocking host call at Close.
	ctx    context.Context
	cancel context.CancelFunc

	// exited broadcasts whenever a process exits, for pgm.wait.
	exited *wasi.Notifier

	mu       sync.Mutex
	programs map[string]program
	procs    map[int32]*proc // every process not yet reaped
	nextPID  int32
	running  int           // processes whose goroutine has not finished
	closed   bool          // no new processes
	idle     chan struct{} // closed once closed is set and running is 0

	rtOnce sync.Once
	rtErr  error
}

// Config configures New.
type Config struct {
	FS       *memfs.FS
	CacheDir string // compiled-module cache; "" disables caching

	// Preempt turns on wazero's close-on-context-done checks on loop
	// back-edges. A guest computing without host calls can then still be
	// killed, and it does not block Go's stop-the-world GC. It is off by
	// default. Postgres yields to the host every 4096th
	// CHECK_FOR_INTERRUPTS (patch 0004), which gives Postgres the same kill
	// and GC latency at no cost. The checks make CPU-bound queries about
	// 2.5x slower. Turn it on for guests that loop without host calls or
	// CHECK_FOR_INTERRUPTS.
	Preempt bool

	// Deterministic makes what the kernel's processes compute independent
	// of the host, its architecture and its speed. It is for building
	// reproducible images (mkcluster), not for running servers.
	//
	//   - Each process's realtime clock starts at 2026-01-01T00:00:00Z and
	//     advances 1µs per read, counted per process.
	//   - Each process's random_get and /dev/urandom read its own fixed
	//     stream, SHA-256 in counter mode over a fixed seed and the pid.
	//     Pids follow spawn order, so a process's bytes do not depend on
	//     when other processes read theirs. An open /dev/urandom keeps
	//     the stream of the process that opened it, so a child that
	//     inherited one would read its parent's. Postgres opens it per
	//     call, so no child inherits one.
	//   - The interval timer (setitimer, alarm) never fires, so no SIGALRM
	//     arrives however much host time passes.
	//   - Guests run on wazero's interpreter, not compiled to machine code.
	//     It is about 40 times slower, and it ignores CacheDir, so every
	//     run converts each module again. In exchange the result cannot
	//     depend on the compiler backend. Under the amd64 compiler, seen
	//     under qemu emulation, initdb's planner chose different plans
	//     from arm64's, and that changed the image.
	//
	// What does not work, or is not fixed:
	//
	//   - Timeouts. The timeouts in Postgres's timeout.c run on SIGALRM, so
	//     they never expire. These are statement_timeout, lock_timeout,
	//     deadlock_timeout and the idle stats update. Timed waits (poll and
	//     sleep timeouts, sem_timedwait) still run on host time, because
	//     the monotonic clock stays real. The kernel measures an absolute
	//     realtime deadline, such as a poll or sem_timedwait abstime,
	//     against the host's clock. A deadline computed from the fixed
	//     clock has therefore already passed.
	//   - File times. memfs stamps mtime and ctime from the host's clock,
	//     and set_times with *_NOW uses it too, so a guest's fstat sees
	//     host times. Images are unaffected, because memfs.WriteTar writes
	//     zero times.
	//   - NaN payloads. The interpreter computes floats with the host's
	//     instructions, and the bits of a NaN that an operation produces
	//     differ between CPUs. An image that stored a computed NaN could
	//     differ across hosts. Today's image stores none, and it is
	//     identical on darwin/arm64 and emulated linux/amd64.
	//
	// When Deterministic is off, processes get the host's clock,
	// crypto/rand, real timers and compiled code.
	Deterministic bool
}

// program is a registered compiled module.
type program struct {
	m           wazero.CompiledModule
	usesPGM     bool // imports from "pgm", so it must pass the ABI check
	canDispatch bool // exports pgmem_signal_dispatch, so the guest handles signals
}

// proc is one process. Fields marked "mu" are guarded by Kernel.mu.
type proc struct {
	pid   int32
	ppid  int32 // mu; becomes 1 when the parent exits first
	owned bool  // started by Kernel.Start, so pid 1 reaps it with Process.Wait
	wasi  *wasi.Proc
	sigs  *signals
	cwd   string // the working directory spawn passed; "" if none

	// cancel ends the process's context, and the process dies at its next
	// host call. termSig is the signal the host killed it with, set before
	// cancel. That is SIGKILL, or a signal whose default action the host
	// carried out for a module without dispatch. exitSig is the signal the
	// guest died of through signal_exit. Each is 0 when there is none.
	cancel  context.CancelFunc
	termSig atomic.Int32
	exitSig atomic.Int32

	// The module's close notifier sets freed when wazero frees the
	// module's resources, memory included.
	freed atomic.Bool

	exited bool          // mu
	status Status        // set before done closes
	done   chan struct{} // closed when the process has exited
}

// New creates a kernel with one runtime, in which pgmem's WASI and the pgm
// module are instantiated.
func New(ctx context.Context, cfg Config) (*Kernel, error) {
	if cfg.FS == nil {
		return nil, errors.New("kernel: Config.FS is nil")
	}
	rt, err := wasmrt.NewRuntime(ctx, wasmrt.Options{CacheDir: cfg.CacheDir, CloseOnContextDone: cfg.Preempt, Interpreter: cfg.Deterministic})
	if err != nil {
		return nil, err
	}
	k := &Kernel{
		rt:       rt,
		fs:       cfg.FS,
		alloc:    new(vmem.Allocator),
		preempt:  cfg.Preempt,
		exited:   wasi.NewNotifier(),
		programs: map[string]program{},
		procs:    map[int32]*proc{},
		nextPID:  firstPID,
		idle:     make(chan struct{}),
	}
	k.deterministic = cfg.Deterministic
	k.shm = newShmTable(k.alloc)
	k.sems = newSemTable()
	k.socks = newSockRegistry()
	k.shm.onFree = k.sems.dropSegment
	k.shm.report = func(pid int32, msg string) {
		k.mu.Lock()
		p := k.procs[pid]
		k.mu.Unlock()
		if p != nil {
			p.report(msg)
		}
	}
	// Processes outlive the ctx New was called with. They end at Close.
	base := experimental.WithMemoryAllocator(context.WithoutCancel(ctx), k.alloc)
	k.ctx, k.cancel = context.WithCancel(base)
	if err := wasi.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	if err := k.instantiatePGM(ctx); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	return k, nil
}

// Close stops the kernel. It refuses new processes and kills every running
// one. It cancels their contexts, which interrupts blocking waits and
// polls. It closes all their fds, which wakes blocked pipe calls. Each
// process then dies with KilledStatus at its next host call, or as the
// current one returns. Close waits until every process has exited or ctx
// is done. The runtime closes once the last process has exited, so no
// instance's memory is freed while it runs. Process.Wait can still reap
// processes that have exited. A process blocked reading a Spec.Stdin
// reader stays blocked until that reader returns. Close stops a process
// that computes without host calls only with Config.Preempt. Without it,
// the process runs until it makes a host call.
func (k *Kernel) Close(ctx context.Context) error {
	k.mu.Lock()
	var live []*proc
	if !k.closed {
		k.closed = true
		if k.running == 0 {
			close(k.idle)
		}
		for _, p := range k.procs {
			if !p.exited {
				live = append(live, p)
			}
		}
	}
	k.mu.Unlock()
	k.cancel()
	for _, p := range live {
		p.wasi.FDs.CloseAll()
	}
	select {
	case <-k.idle:
	case <-ctx.Done():
		return ctx.Err()
	}
	return k.closeRuntime()
}

// closeRuntime closes the runtime once, then frees every shared-memory
// segment. Every caller gets the runtime's error.
func (k *Kernel) closeRuntime() error {
	k.rtOnce.Do(func() {
		k.rtErr = k.rt.Close(context.Background())
		k.shm.close()
	})
	return k.rtErr
}

// Compile compiles a module for Register.
func (k *Kernel) Compile(ctx context.Context, wasm []byte) (wazero.CompiledModule, error) {
	return k.rt.CompileModule(ctx, wasm)
}

// Register makes a compiled module runnable as the given program path,
// such as "/pgmem/bin/postgres". It refuses a module that imports from
// "pgm" but does not export pgmem_abi_version.
func (k *Kernel) Register(path string, m wazero.CompiledModule) error {
	exports := m.ExportedFunctions()
	if f, ok := exports["_start"]; !ok || len(f.ParamTypes()) != 0 || len(f.ResultTypes()) != 0 {
		return fmt.Errorf("kernel: %s: module does not export _start() as a command does", path)
	}
	f, ok := exports[dispatchExport]
	canDispatch := ok && len(f.ParamTypes()) == 0 && len(f.ResultTypes()) == 0
	usesPGM := false
	for _, f := range m.ImportedFunctions() {
		if mod, _, _ := f.Import(); mod == pgmModule {
			usesPGM = true
			break
		}
	}
	if usesPGM {
		f, ok := exports[abiExport]
		if !ok || len(f.ParamTypes()) != 0 || !slices.Equal(f.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return fmt.Errorf("kernel: %s: module imports from %q but does not export %s() -> i32", path, pgmModule, abiExport)
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.programs[path] = program{m: m, usesPGM: usesPGM, canDispatch: canDispatch}
	return nil
}

func (k *Kernel) program(path string) (program, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	prog, ok := k.programs[path]
	return prog, ok
}

// ErrClosed is the error Start returns once the kernel is closed.
var ErrClosed = errors.New("kernel: closed")

// Spec describes a process started from Go.
//
// The guest sees Stdin, Stdout and Stderr as a terminal. isatty is true
// for fds 0-2 even when they are pipes or buffers in Go. So libc
// line-buffers stdout instead of buffering it fully, and Postgres colours
// its messages under PG_COLOR=auto.
type Spec struct {
	Path           string   // a registered program path
	Args           []string // argv after argv[0]
	Env            []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Start runs a program as a child of pid 1, the Go caller. Its fds 0-2 are
// the Spec's streams, and fd 3 is the root preopen. A nil Stdin reads EOF,
// and a nil Stdout or Stderr discards. Start returns an error if the path
// is not registered, if argv and environment exceed 2 MiB (wrapping
// wasiabi.E2BIG), if the module cannot be instantiated, or if the kernel
// is closed (ErrClosed). A module that fails the pgm ABI check is not an
// error. The process exits 127 and says why on its stderr.
func (k *Kernel) Start(ctx context.Context, s Spec) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prog, ok := k.program(s.Path)
	if !ok {
		return nil, fmt.Errorf("kernel: %s is not a registered program", s.Path)
	}
	args := append([]string{s.Path}, s.Args...)
	if argBytes(args)+argBytes(s.Env) > maxArgBytes {
		return nil, fmt.Errorf("kernel: start %s: argument list and environment exceed %d bytes: %w", s.Path, maxArgBytes, wasiabi.E2BIG)
	}
	fds := wasi.NewRootFDs(k.fs, s.Stdin, s.Stdout, s.Stderr)
	p, errno, err := k.start(initPID, true, prog, args, slices.Clone(s.Env), "", 0, fds)
	if err != nil {
		return nil, fmt.Errorf("kernel: start %s: %w", s.Path, err)
	}
	if errno != wasiabi.ESUCCESS {
		return nil, fmt.Errorf("kernel: start %s: %w", s.Path, ErrClosed)
	}
	return &Process{k: k, p: p}, nil
}

// argBytes is the size of strs as NUL-terminated strings.
func argBytes(strs []string) int {
	n := 0
	for _, s := range strs {
		n += len(s) + 1
	}
	return n
}

// Process is a process started from Go.
type Process struct {
	k *Kernel
	p *proc
}

// PID returns the process's pid.
func (p *Process) PID() int32 { return p.p.pid }

// ErrProcessDone is the error Process.Signal returns once the process has
// exited.
var ErrProcessDone = errors.New("kernel: process has exited")

// Signal sends sig to the process. It is kill on the Go side. SIGKILL
// kills the process at its next host call. With Config.Preempt it also
// kills it in the middle of a loop that makes no host calls. In a module
// without the compat layer's dispatch, any signal whose default action
// terminates does the same. sig 0 checks that the process exists. Signal
// returns ErrProcessDone if the process has exited. Signal numbers are
// wasi-libc's, 0 to 64.
func (p *Process) Signal(sig int) error {
	if sig < 0 || sig > maxSignal {
		return fmt.Errorf("kernel: signal %d out of range", sig)
	}
	select {
	case <-p.p.done:
		return ErrProcessDone
	default:
	}
	if sig != 0 {
		p.p.signal(sig)
	}
	return nil
}

// Wait blocks until the process exits and reaps it. Later calls return the
// same status.
func (p *Process) Wait() Status {
	<-p.p.done
	k := p.k
	k.mu.Lock()
	if k.procs[p.p.pid] == p.p {
		delete(k.procs, p.p.pid)
	}
	k.mu.Unlock()
	return p.p.status
}

// lookup returns the process pid and whether it has exited. The process
// is nil if pid never started, or if it exited and was reaped. The answer
// can change once k.mu is released. Senders signal outside the lock, as
// pgm.kill does.
func (k *Kernel) lookup(pid int32) (p *proc, exited bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p = k.procs[pid]
	return p, p != nil && p.exited
}

// Signal sends sig to process pid, like pgm.kill. It reaches any process,
// not only those Go started. Unlike pgm.kill, it returns ErrProcessDone
// for pid 1, zombies and pids that never existed, as os.Process.Signal
// does. sig 0 checks that the process exists. Signal numbers are
// wasi-libc's, 0 to 64.
func (k *Kernel) Signal(pid int32, sig int) error {
	if sig < 0 || sig > maxSignal {
		return fmt.Errorf("kernel: signal %d out of range", sig)
	}
	p, exited := k.lookup(pid)
	if p == nil || exited {
		return ErrProcessDone
	}
	if sig != 0 {
		p.signal(sig)
	}
	return nil
}

// ProcCount returns the number of processes that have not exited.
func (k *Kernel) ProcCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, p := range k.procs {
		if !p.exited {
			n++
		}
	}
	return n
}

// start creates a process running prog and takes over the fd table fds.
// cwd is the working directory the process's compat layer moves to at
// startup, or "" for none. blocked is the signal mask the process starts
// with. A spawned child inherits its parent's mask, as across fork and
// exec. start fails with EAGAIN when the kernel is closed.
//
// If the module cannot be instantiated, a process Go starts (owned) gets
// an error. A guest's spawn instead exits at once with status 127 and says
// why on its stderr. If instantiation panics because vmem cannot reserve
// the memory, start fails with ENOMEM and the error in both cases, and no
// process remains. A module that fails the ABI check exits 127 in both
// cases.
func (k *Kernel) start(ppid int32, owned bool, prog program, args, env []string, cwd string, blocked uint64, fds *wasi.FDTable) (*proc, wasiabi.Errno, error) {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		fds.CloseAll()
		return nil, wasiabi.EAGAIN, nil
	}
	ctx, cancel := context.WithCancel(k.ctx)
	p := &proc{
		pid:    k.nextPID,
		ppid:   ppid,
		owned:  owned,
		cwd:    cwd,
		wasi:   &wasi.Proc{FS: k.fs, FDs: fds, Args: args, Env: env},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if k.deterministic {
		p.wasi.Realtime = newFixedClock()
		p.wasi.Random = newFixedRandom(procSeed(p.pid))
	}
	p.sigs = newSignals(p, prog.canDispatch, blocked)
	p.wasi.Signals = p.sigs
	p.wasi.AfterCall = p.sigs.afterCall
	k.nextPID++
	k.procs[p.pid] = p
	k.running++
	k.mu.Unlock()

	ctx = wasi.WithProc(withProc(ctx, p), p.wasi)
	// Every close of the module frees its memory, whoever closes it:
	// proc_exit, a kill, or exit below. The notifier detaches the signal
	// block first.
	ctx = experimental.WithCloseNotifier(ctx, experimental.CloseNotifyFunc(func(context.Context, uint32) {
		p.sigs.detach()
		p.freed.Store(true)
	}))
	cfg := wazero.NewModuleConfig().WithName(fmt.Sprintf("pid-%d", p.pid)).WithStartFunctions()
	mod, err := k.instantiate(ctx, prog.m, cfg)
	if panicked := errors.Is(err, errInstantiatePanic); err != nil && (owned || panicked) {
		k.exit(ctx, p, mod, ExitStatus(execFailed))
		k.mu.Lock()
		delete(k.procs, p.pid)
		k.mu.Unlock()
		cancel()
		if panicked {
			return nil, wasiabi.ENOMEM, err
		}
		return nil, wasiabi.ESUCCESS, err
	}
	if err == nil && prog.usesPGM {
		err = checkABI(ctx, mod)
	}
	if err != nil {
		p.report(fmt.Sprintf("pgmem: %s: cannot start: %v\n", args[0], err))
		k.exit(ctx, p, mod, ExitStatus(execFailed))
		cancel()
		return p, wasiabi.ESUCCESS, nil
	}
	go k.run(ctx, cancel, p, mod)
	return p, wasiabi.ESUCCESS, nil
}

// errInstantiatePanic marks an instantiation that panicked.
var errInstantiatePanic = errors.New("instantiation panicked")

// instantiate instantiates m, turning a panic into an error wrapping
// errInstantiatePanic. vmem's allocator panics when it cannot reserve a
// memory, because wazero gives it no error path. wazero registers a module
// only once instantiation has succeeded, so nothing is left behind.
func (k *Kernel) instantiate(ctx context.Context, m wazero.CompiledModule, cfg wazero.ModuleConfig) (mod api.Module, err error) {
	defer func() {
		if v := recover(); v != nil {
			mod, err = nil, fmt.Errorf("%w: %v", errInstantiatePanic, v)
		}
	}()
	return k.rt.InstantiateModule(ctx, m, cfg)
}

// checkABI refuses a module built for another version of pgm.
func checkABI(ctx context.Context, mod api.Module) error {
	res, err := mod.ExportedFunction(abiExport).Call(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", abiExport, err)
	}
	if v := api.DecodeI32(res[0]); v != ABIVersion {
		return fmt.Errorf("built for pgm ABI version %d; this kernel runs version %d", v, ABIVersion)
	}
	return nil
}

// isKillCode reports whether code is how a module ends when its context is
// cancelled: the host's own kill code, or wazero's context-done code when
// preemption stops a guest between host calls.
func isKillCode(code uint32) bool {
	return code == wasi.KillExitCode || code == sys.ExitCodeContextCanceled
}

// run calls _start and turns its outcome into the process's status.
func (k *Kernel) run(ctx context.Context, cancel context.CancelFunc, p *proc, mod api.Module) {
	defer cancel()
	_, err := mod.ExportedFunction("_start").Call(ctx)
	status := ExitStatus(0)
	termSig, exitSig := p.termSig.Load(), p.exitSig.Load()
	var exitErr *sys.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr) && exitSig != 0 && exitErr.ExitCode() == uint32(128+exitSig):
		// signal_exit carried out a signal's default action in the guest.
		status = Status(exitSig)
	case errors.As(err, &exitErr) && termSig != 0 && isKillCode(exitErr.ExitCode()) && ctx.Err() != nil:
		// The host carried out a signal's default action. That is SIGKILL,
		// or any signal for a module without dispatch. A process that
		// exited on its own as the signal came keeps its code.
		status = Status(termSig)
	case errors.As(err, &exitErr) && isKillCode(exitErr.ExitCode()) && ctx.Err() != nil:
		// A host call, or wazero's preemption check in a guest loop, ended
		// the process because Close cancelled its context. run itself
		// cancels it only on return.
		status = KilledStatus
	case errors.As(err, &exitErr):
		status = ExitStatus(exitErr.ExitCode())
	default:
		status = TrapStatus
		p.report(fmt.Sprintf("pgmem: %s (pid %d) trapped: %v\n", p.wasi.Args[0], p.pid, err))
	}
	k.exit(ctx, p, mod, status)
}

// exit finishes a process. It detaches the signal block and stops the
// timer before the module's memory is freed, so no sender writes into it.
// It releases the process's shared-memory attachments and drops its
// private semaphores. It closes the module, if there is one, and every fd,
// so peers see EOF and EPIPE. The process becomes a zombie, and its
// children pass to pid 1.
//
// An orphan is a process whose parent is pid 1 but that Go did not start.
// exit reaps it at once, as init would, since nothing could wait for it. A
// parent that is a process gets SIGCHLD. As on Linux, the signal is
// pending before any wait can reap the zombie, so a handler that reaps,
// such as Postgres's postmaster, sees the child.
func (k *Kernel) exit(ctx context.Context, p *proc, mod api.Module, status Status) {
	p.sigs.detach()
	// Attachments end with the process. Only the bookkeeping changes,
	// because the mappings go away with its memory.
	k.shm.release(p.pid)
	k.sems.dropProcess(p.pid)
	if mod != nil {
		_ = mod.Close(ctx)
		k.freeDeferred(ctx, p, mod)
	}
	p.wasi.FDs.CloseAll()

	k.mu.Lock()
	p.status, p.exited = status, true
	for _, c := range k.procs {
		if c.ppid == p.pid {
			c.ppid = initPID
			if c.exited {
				delete(k.procs, c.pid)
			}
		}
	}
	if p.ppid == initPID && !p.owned {
		delete(k.procs, p.pid)
	}
	if parent := k.procs[p.ppid]; p.ppid != initPID && parent != nil && !parent.exited {
		// Lock order: Kernel.mu, then signals.mu. SIGCHLD is ignored by
		// default, so send never ends the parent itself.
		parent.sigs.send(sigCHLD)
	}
	k.running--
	last := k.closed && k.running == 0
	if last {
		close(k.idle)
	}
	close(p.done)
	k.mu.Unlock()
	k.exited.Broadcast()

	if last {
		// Close gave up waiting, or is about to see idle. Either way the
		// runtime closes now that no instance runs.
		_ = k.closeRuntime()
	}
}

// freeDeferred frees the resources of a closed module whose close wazero
// deferred and then never carried out.
//
// With Config.Preempt, wazero watches each call's context. When the
// context is cancelled, the watcher goroutine marks the module closed. It
// leaves freeing the module's resources, above all its memory, to the next
// FailIfClosed on the calling goroutine. A killed process's host call
// closes the module itself (wasi.ExitIfKilled) and panics to unwind the
// guest. If the watcher won the race, that CloseWithExitCode is a no-op.
// wazevo's recover path calls FailIfClosed only when the call returned
// without a panic. See callWithStack in
// internal/engine/wazevo/call_engine.go in wazero v1.12.0. The pinned main
// has the same recover path. So the deferred free never happens. mod.Close
// is a no-op on a closed module, and the watcher already removed the
// module from the store, so Runtime.Close misses it too. Every killed
// process would leak its memory.
//
// Calling any export with a context that is already cancelled takes the
// path that does free them. wazevo checks the context before entering the
// guest and finds the module closed, so CloseWithCtxErr is a no-op. It
// returns FailIfClosed, which frees the deferred resources. No guest code
// runs. Without preemption, wazero would run the guest instead, but then
// nothing defers a close. The recover path should call FailIfClosed too,
// and that is worth an upstream issue.
func (k *Kernel) freeDeferred(ctx context.Context, p *proc, mod api.Module) {
	if p.freed.Load() || !k.preempt || !mod.IsClosed() {
		return
	}
	f := mod.ExportedFunction("_start") // every registered program has one
	if f == nil {
		return
	}
	dead, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = f.Call(dead)
}

// report writes msg to the process's stderr without blocking.
func (p *proc) report(msg string) {
	if d, err := p.wasi.FDs.Get(2); err == wasiabi.ESUCCESS {
		d.File().Write([]byte(msg), wasiabi.FdflagNonblock)
	}
}

type procKey struct{}

func withProc(ctx context.Context, p *proc) context.Context {
	return context.WithValue(ctx, procKey{}, p)
}

func procFrom(ctx context.Context) *proc {
	p, _ := ctx.Value(procKey{}).(*proc)
	return p
}
