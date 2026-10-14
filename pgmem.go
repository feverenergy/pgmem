// Package pgmem runs PostgreSQL 17, compiled to WebAssembly, inside the Go
// process. Start boots a real postmaster on an in-memory file system, and
// clients connect through Server.Dial.
package pgmem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feverenergy/pgmem/internal/buildid"
	"github.com/feverenergy/pgmem/internal/cluster"
	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/memfs"
	"github.com/feverenergy/pgmem/internal/pgwire"
	"github.com/feverenergy/pgmem/internal/serverfs"
	"github.com/feverenergy/pgmem/internal/wasiabi"
	"github.com/feverenergy/pgmem/internal/wasmrt"
)

// Options configures Start.
type Options struct {
	// Engine is the PostgreSQL build to run. The zero Engine means
	// Postgres17. $PGMEM_BUILD_DIR, when set, replaces it.
	Engine Engine

	Image []byte // a cluster image (tar of the data directory); nil = the build's cluster.tar

	// Settings are extra settings, passed as -c name=value. Start rejects
	// the ones pgmem sets itself, which place the data directory, its
	// configuration files and the socket Dial connects to: config_file,
	// data_directory, hba_file, listen_addresses, port and
	// unix_socket_directories.
	Settings map[string]string

	// Log, if set, also receives the server log in chunks of whole lines,
	// from a single goroutine. It never blocks the server. While Log is
	// more than 256 chunks behind, pgmem drops new chunks, and Server.Log
	// still has them. Once Close or a failing Start has closed the kernel,
	// pgmem discards queued chunks and waits up to 1 s for a Write in
	// progress, then abandons it. So pgmem never calls Log after Close
	// returns, apart from that one Write if it is still blocked.
	Log io.Writer
}

const (
	pidFile  = cluster.DataDir + "/postmaster.pid"
	sockDir  = "/tmp" // the image's unix_socket_directories
	sockPath = sockDir + "/.s.PGSQL.5432"

	// shmemAddr is where the compat layer maps System V shared memory in
	// every process: above the heap's end (about 11 MiB), low enough that
	// each process reserves little address space beyond it.
	shmemAddr = "0x2000000"

	readyPoll = 2 * time.Millisecond // how often Start reads postmaster.pid
	logLines  = 30                   // log lines quoted when Start fails
)

// The shutdown sequence's bounds (Close and Shutdown). Fast shutdown
// (SIGINT) took 120 to 150 ms with sessions open, and immediate shutdown
// (SIGQUIT) 3 ms, so these leave a wide margin. pgmem never uses smart
// shutdown (SIGTERM), because it waits forever on idle pooled
// connections. Last, Close waits up to logCloseWait for a pending
// Options.Log write. They are variables only so that tests can shorten
// them.
var (
	fastShutdownWait = 5 * time.Second // after SIGINT
	quitWait         = 2 * time.Second // after SIGQUIT
	kernelCloseWait  = 5 * time.Second // for Kernel.Close
	logCloseWait     = 1 * time.Second // for the log copier's Write
)

// reservedSettings are the settings Start refuses in Options.Settings:
// changing them would move the data directory or its configuration files
// away from the image, or the socket away from where Dial looks.
var reservedSettings = []string{
	"config_file", "data_directory", "hba_file",
	"listen_addresses", "port", "unix_socket_directories",
}

// Signal numbers (wasi-libc's, as Linux's).
const (
	sigINT  = 2
	sigQUIT = 3
)

// init hands two values to the module's own packages. cmd/pgmem-serve gets
// a server's file system and pgtest gets the build ID. Both go through
// internal packages so that they stay out of the public API.
func init() {
	serverfs.FS = func(s any) *memfs.FS { return s.(*Server).fs }
	buildid.ID = func(version string, fsys fs.FS) (string, error) {
		return buildID(Engine{Version: version, FS: fsys})
	}
}

// Server is a running Postgres server. Its methods are safe for concurrent
// use.
type Server struct {
	k   *kernel.Kernel
	fs  *memfs.FS
	log *serverLog

	pm       *kernel.Process
	pmDone   chan struct{} // closed when the postmaster has exited
	pmStatus kernel.Status // set before pmDone closes

	closing   atomic.Bool // Close or Shutdown has begun: Dial refuses
	closeOnce sync.Once   // runs shutdown once, for Close or Shutdown
	closeErr  error       // Close's result: the kernel close's error
}

// Start boots a postmaster on a fresh copy of the cluster image and
// returns once it accepts connections. If the postmaster exits first, the
// error quotes the end of its log. If ctx is done first, Start stops it and
// returns ctx's error. ctx bounds only the start: the server runs until
// Close.
//
// Only ctx bounds how long Start waits for the server to become ready, so
// give it a deadline. A start takes well under a second once the compile
// cache is warm. Start fails at once if ctx is already done, and if
// Options.Settings names a reserved setting. If the compile cache
// directory cannot be created or written, Start compiles without it and
// says so in the server log.
//
// Start runs Options.Engine: Postgres, its share files and, unless
// Options.Image is set, the engine's fresh cluster image. The first Start
// with an engine decompresses its files. They stay in memory for the life
// of the process, about 42 MB for Postgres17, and every server on that
// engine shares them. When a file is missing or corrupt, the error names
// the engine's Version and the file, and a missing file's error wraps
// fs.ErrNotExist. When $PGMEM_BUILD_DIR is set (use an absolute path),
// Start ignores the engine and reads postgres.wasm, share.tar and
// cluster.tar from that directory (make wasm cluster) on every call; when
// one is missing, its error wraps fs.ErrNotExist. It does not check that
// the directory's postgres.wasm and cluster.tar belong together: after
// make wasm, run make cluster too.
func Start(ctx context.Context, opts Options) (*Server, error) {
	return start(ctx, opts, false)
}

// start is Start; preempt sets kernel.Config.Preempt (tests and benchmarks
// compare the two).
func start(ctx context.Context, opts Options, preempt bool) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pgmem: start: %w", err)
	}
	if err := checkSettings(opts.Settings); err != nil {
		return nil, err
	}
	b, image, err := loadBuild(opts.Engine, opts.Image)
	if err != nil {
		return nil, err
	}
	log := newServerLog(opts.Log)
	fsys := memfs.New()
	k, err := kernel.New(ctx, kernel.Config{FS: fsys, CacheDir: compileCache(log), Preempt: preempt})
	if err != nil {
		log.close(logCloseWait)
		return nil, fmt.Errorf("pgmem: start: %w", err)
	}
	s := &Server{k: k, fs: fsys, log: log, pmDone: make(chan struct{})}
	if quoteLog, err := s.boot(ctx, b, image, opts.Settings); err != nil {
		s.closeKernel() // after this, the log holds all the server wrote
		if quoteLog {
			err = fmt.Errorf("%w; last log lines:\n%s", err, s.log.tail(logLines))
		}
		return nil, err
	}
	return s, nil
}

// checkSettings rejects a reserved setting. Postgres matches setting
// names ignoring case, and -c turns dashes into underscores.
func checkSettings(settings map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(settings)) {
		norm := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
		if slices.Contains(reservedSettings, norm) {
			return fmt.Errorf("pgmem: start: setting %q is reserved: pgmem sets it (data in %s, socket %s)", name, cluster.DataDir, sockPath)
		}
	}
	return nil
}

// cacheDir returns the compile cache directory; tests replace it.
var cacheDir = wasmrt.DefaultCacheDir

// compileCache returns the compile cache directory, or "" for none: when
// there is no cache directory, or it cannot be created or written. Without
// it, modules compile on every Start, which costs about a second but works.
// compileCache reports an unusable directory with one line in log.
func compileCache(log io.Writer) string {
	dir, err := cacheDir()
	if err != nil {
		return ""
	}
	if err := usableDir(dir); err != nil {
		fmt.Fprintf(log, "pgmem: compiling without a cache: %v\n", err)
		return ""
	}
	return dir
}

// usableDir creates dir if it is missing and checks a file can be created
// in it, as wazero's cache will need.
func usableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "probe-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// boot installs the build and the image, starts the postmaster and waits
// until it is ready. On error, the caller closes the kernel and, if
// quoteLog is set, adds the end of the server log to the error.
func (s *Server) boot(ctx context.Context, b cluster.Build, image []byte, settings map[string]string) (quoteLog bool, err error) {
	if err := cluster.Install(ctx, s.k, s.fs, b); err != nil {
		return false, fmt.Errorf("pgmem: start: %w", err)
	}
	if err := s.fs.LoadTar(bytes.NewReader(image), cluster.DataDir); err != nil {
		return false, fmt.Errorf("pgmem: start: load image: %w", err)
	}
	// An image taken from a running server carries its lock file.
	if errno := s.fs.Unlink(s.fs.Root(), pidFile); errno != wasiabi.ESUCCESS && errno != wasiabi.ENOENT {
		return false, fmt.Errorf("pgmem: start: remove %s: %w", pidFile, errno)
	}
	if errno := s.fs.Mkdir(s.fs.Root(), sockDir); errno != wasiabi.ESUCCESS && errno != wasiabi.EEXIST {
		return false, fmt.Errorf("pgmem: start: mkdir %s: %w", sockDir, errno)
	}

	args := []string{"-D", cluster.DataDir}
	for _, name := range slices.Sorted(maps.Keys(settings)) {
		args = append(args, "-c", name+"="+settings[name])
	}
	pm, err := s.k.Start(ctx, kernel.Spec{
		Path:   cluster.PostgresPath,
		Args:   args,
		Env:    []string{"PG_SHMEM_ADDR=" + shmemAddr, "TZ=UTC"},
		Stdout: s.log,
		Stderr: s.log,
	})
	if err != nil {
		return false, fmt.Errorf("pgmem: start: %w", err)
	}
	s.pm = pm
	go func() {
		s.pmStatus = pm.Wait()
		close(s.pmDone)
	}()

	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	for !s.ready() {
		select {
		case <-s.pmDone:
			return true, fmt.Errorf("pgmem: start: postgres %s before it was ready", describe(s.pmStatus))
		case <-ctx.Done():
			return true, fmt.Errorf("pgmem: start: %w", ctx.Err())
		case <-tick.C:
		}
	}
	return false, nil
}

// ready reports whether postmaster.pid's status line (line 8) says
// "ready". A missing or partly written file is not ready.
func (s *Server) ready() bool {
	data, err := s.fs.ReadFile(pidFile)
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	return len(lines) > 7 && strings.TrimSpace(lines[7]) == "ready"
}

// describe says how a process ended.
func describe(st kernel.Status) string {
	switch {
	case st.Exited():
		return fmt.Sprintf("exited with code %d", st.ExitCode())
	case st.Signaled():
		return fmt.Sprintf("was killed by signal %d", st.Signal())
	default:
		return fmt.Sprintf("ended with status %#x", int32(st))
	}
}

// Dial connects to the server's Unix socket. It ignores network and addr,
// so it works as pgx's DialFunc for both connections and
// CancelRequests (pgx passes host /tmp). After Close it returns an error
// wrapping net.ErrClosed.
func (s *Server) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if s.closing.Load() {
		return nil, &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: sockPath, Net: "unix"}, Err: net.ErrClosed}
	}
	return s.k.Dial(ctx, sockPath)
}

// Snapshot returns an image of the data directory, which Options.Image
// accepts. It runs CHECKPOINT, then copies the files while it holds the
// file system still, so every write waits until the copy is done. A
// Snapshot took about 10 ms on an idle server and 17 to 48 ms while
// clients were writing.
// The image is crash-consistent: a server started from it boots through a
// short crash recovery. So:
//
//   - every commit to a logged relation made before the call is in the
//     image, whatever synchronous_commit was (CHECKPOINT flushes the WAL);
//     commits made during the call may or may not be;
//   - unlogged tables come back empty, and unlogged sequences reset;
//   - cumulative statistics (pg_stat_*) are reset.
//
// After Close or Shutdown, or when one begins while it runs, it returns an
// error wrapping net.ErrClosed.
func (s *Server) Snapshot(ctx context.Context) ([]byte, error) {
	img, err := s.snapshot(ctx)
	if err != nil && s.closing.Load() && !errors.Is(err, net.ErrClosed) {
		// Close ended the CHECKPOINT's session or the server under it.
		err = fmt.Errorf("pgmem: snapshot: %w (%v)", net.ErrClosed, err)
	}
	return img, err
}

func (s *Server) snapshot(ctx context.Context) ([]byte, error) {
	conn, err := s.Dial(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("pgmem: snapshot: %w", err)
	}
	if err := pgwire.Exec(ctx, conn, "postgres", "postgres", "CHECKPOINT"); err != nil {
		return nil, fmt.Errorf("pgmem: snapshot: checkpoint: %w", err)
	}
	img, err := s.dataImage()
	if err != nil {
		return nil, fmt.Errorf("pgmem: snapshot: %w", err)
	}
	return img, nil
}

// dataImage returns a tar of the data directory, as Options.Image accepts.
func (s *Server) dataImage() ([]byte, error) {
	var buf bytes.Buffer
	if err := s.fs.WriteTar(&buf, cluster.DataDir); err != nil {
		return nil, err
	}
	// The buffer grew by doubling: copy the image out so that the caller
	// does not hold up to twice its size.
	img := make([]byte, buf.Len())
	copy(img, buf.Bytes())
	return img, nil
}

// Log returns the most recent server log, up to LogKeep bytes.
func (s *Server) Log() string { return s.log.String() }

// Close shuts the server down: fast shutdown (SIGINT), which ends open
// sessions; then, if the postmaster is still running, immediate shutdown
// (SIGQUIT); then the kernel closes, ending any process left. Each
// escalation adds a line to the server log. It returns an error only if
// the kernel could not close within 5 s; the kernel then goes on closing
// in the background, freeing the runtime once its last process ends.
// Later calls return the same result. After Shutdown, Close returns nil, or
// the error of Shutdown's kernel close.
func (s *Server) Close() error {
	s.closeOnce.Do(func() { s.shutdown(context.Background(), false) })
	return s.closeErr
}

// Shutdown shuts the server down as Close does and returns a clean-shutdown
// image of the data directory, which Options.Image accepts.
//
// It sends SIGINT (fast shutdown, which ends open sessions) and waits for
// the postmaster to exit, for up to 5 s and no longer than ctx allows. If
// the postmaster exited with code 0, it returns an image of the data
// directory, taken before the kernel closes. That image holds no
// postmaster.pid, and a server started from it skips crash recovery:
// unlogged tables keep their rows, unlogged sequences their state, and
// cumulative statistics (pg_stat_*) survive. Unlike Snapshot, no session
// can be running, so every commit is in the image.
//
// Otherwise Shutdown escalates exactly as Close does. It returns no image,
// and an error that quotes the end of the server log. That happens when the
// postmaster did not exit in time, ctx was done first, or it exited with
// another status.
// A done ctx does not stop Shutdown: it skips the wait for fast shutdown.
// The escalation's log line names its cause: the wait timing out, or ctx.
//
// Either way the server is closed afterwards: a later Close returns nil, or
// the error of the kernel's close (which does not affect the image).
// Shutdown after Close, or a second Shutdown, returns an error wrapping
// net.ErrClosed.
func (s *Server) Shutdown(ctx context.Context) ([]byte, error) {
	img, err := []byte(nil), fmt.Errorf("pgmem: shutdown: %w", net.ErrClosed)
	s.closeOnce.Do(func() { img, err = s.shutdown(ctx, true) })
	return img, err
}

// shutdown is the shutdown sequence that Close and Shutdown share; it runs
// once, under closeOnce. It sends SIGINT and waits up to fastShutdownWait,
// and while ctx allows, for the postmaster to exit; then, if it has not,
// escalates to SIGQUIT and, if that does not end it within quitWait,
// leaves the rest to the kernel's close. It sets closeErr from the kernel's
// close. With image set, it returns the data directory's image if the
// postmaster exited with code 0 after SIGINT, and otherwise an error that
// quotes the end of the log.
func (s *Server) shutdown(ctx context.Context, image bool) ([]byte, error) {
	s.closing.Store(true)
	stopErr := s.stopPostmaster(ctx, sigINT, fastShutdownWait)
	if stopErr != nil {
		cause := fmt.Sprintf("timed out after %v", fastShutdownWait)
		if ctx.Err() != nil && errors.Is(stopErr, ctx.Err()) {
			cause = fmt.Sprintf("cut short (%v)", stopErr)
		}
		fmt.Fprintf(s.log, "pgmem: fast shutdown %s; sent SIGQUIT\n", cause)
		if s.stopPostmaster(context.Background(), sigQUIT, quitWait) != nil {
			fmt.Fprintln(s.log, "pgmem: immediate shutdown timed out; killing remaining processes")
		}
	}
	var img []byte
	var err error
	quoteLog := true
	if image {
		switch {
		case stopErr != nil:
			err = fmt.Errorf("pgmem: shutdown: fast shutdown did not finish: %w", stopErr)
		case !s.pmStatus.Exited() || s.pmStatus.ExitCode() != 0:
			err = fmt.Errorf("pgmem: shutdown: postgres %s", describe(s.pmStatus))
		default:
			quoteLog = false
			if img, err = s.dataImage(); err != nil {
				err = fmt.Errorf("pgmem: shutdown: %w", err)
			}
		}
	}
	if kerr := s.closeKernel(); kerr != nil { // after this, the log holds all the server wrote
		s.closeErr = fmt.Errorf("pgmem: close: %w", kerr)
	}
	if err != nil && quoteLog {
		err = fmt.Errorf("%w; last log lines:\n%s", err, s.log.tail(logLines))
	}
	return img, err
}

// stopPostmaster sends sig to the postmaster and waits for it to exit. It
// returns nil if it exited within wait and before ctx was done, and
// otherwise ctx's error or a timeout error.
func (s *Server) stopPostmaster(ctx context.Context, sig int, wait time.Duration) error {
	if err := s.pm.Signal(sig); errors.Is(err, kernel.ErrProcessDone) {
		<-s.pmDone // exited; the Wait goroutine is about to close it
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-s.pmDone:
		return nil
	case <-t.C:
		return fmt.Errorf("postgres did not exit within %v", wait)
	case <-ctx.Done():
		select {
		case <-s.pmDone: // both were ready: the exit counts
			return nil
		default:
			return ctx.Err()
		}
	}
}

// closeKernel closes the kernel, waiting up to kernelCloseWait for its
// processes to end, then stops the log copier, waiting up to logCloseWait.
// If the kernel does not close in time, a goroutine waits for it without a
// bound, so the runtime is still closed and its memory freed once the last
// process ends.
func (s *Server) closeKernel() error {
	ctx, cancel := context.WithTimeout(context.Background(), kernelCloseWait)
	defer cancel()
	err := s.k.Close(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		go func() {
			err := s.k.Close(context.Background())
			if backgroundClosed != nil {
				backgroundClosed(err)
			}
		}()
	}
	s.log.close(logCloseWait)
	return err
}

// backgroundClosed, if set, receives the result of a kernel close that
// closeKernel left running in the background (tests).
var backgroundClosed func(error)
