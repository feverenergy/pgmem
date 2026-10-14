// Command pgmem-serve exposes a pgmem server on a local socket, for native
// tools such as pg_regress and psql. Each accepted connection is proxied
// byte for byte to Server.Dial.
//
//	pgmem-serve [-socket DIR] [-port N] [-tcp ADDR] [-in DIR]... [-out DIR]...
//	            [-lib-suffix SUFFIX]... [-set name=value]... [-log FILE]
//	            [-max-rss-mb N] [-stats FILE]
//
// It listens on the Unix socket DIR/.s.PGSQL.N, where DIR is a new
// temporary directory under /tmp unless -socket names one, and prints
// "ready <socket path>" on stdout once it accepts connections. -tcp also
// listens on a loopback TCP address.
//
// -in DIR copies the host tree DIR into memfs at the same absolute path,
// for server-side inputs (COPY FROM, pg_read_file). -out DIR creates DIR in
// memfs and keeps it in sync with the host directory, both ways, around
// queries whose text names DIR. pgmem-serve copies the host directory into
// memfs before it forwards such a query, and copies memfs back to the host
// before the ReadyForQuery that follows the query.
// pgmem-serve inspects only simple Query and Parse messages. In the
// extended protocol, executing an already prepared statement (Bind/Execute
// alone) does not sync, and neither does a query text over 1 MiB. One flag
// per connection arms the sync, so a client that pipelines queries may see
// it run at an earlier query's ReadyForQuery instead. psql and pg_regress
// do not pipeline. On the host side, the sync never follows a symlink
// inside DIR.
// Neither flag accepts /, /tmp, or a path under /dev, /pgmem or /pgdata.
// -lib-suffix adds <lib>SUFFIX placeholders next to the .so placeholders of
// the statically linked libraries, for a client that passes its own
// platform's suffix (pg_regress on macOS passes .dylib).
//
// The server trusts every connection (trust authentication), and its only
// user is the superuser postgres. Anyone who can reach the socket or the
// -tcp port is superuser. With -out, superuser can write files in the -out
// directories as the user running pgmem-serve, because a COPY TO a file
// there reaches the host through the sync. The default socket directory is
// private (mode 0700). A -socket directory or a -tcp port is open to every
// local user who can reach it.
//
// A watchdog samples the process's RSS every 250 ms (on Linux, its
// proportional set size). Past -max-rss-mb it closes the server and exits
// with status 1. SIGINT and SIGTERM close the server, then write the
// -stats file. A second signal exits at once with status 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/feverenergy/pgmem"
	"github.com/feverenergy/pgmem/internal/serverfs"
)

// maxSockPath is the longest Unix socket path pgmem-serve accepts. macOS's
// sun_path holds 104 bytes with the terminating NUL, and Linux's holds
// 108.
const maxSockPath = 103

const startTimeout = 60 * time.Second

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go handleSignals(sig, cancel, os.Stderr, os.Exit)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgmem-serve:", err)
		os.Exit(1)
	}
}

// handleSignals cancels run's context at the first signal, and exits with
// status 2 at a second one, for a shutdown that hangs.
func handleSignals(sig <-chan os.Signal, cancel context.CancelFunc, stderr io.Writer, exit func(int)) {
	<-sig
	cancel()
	s := <-sig
	fmt.Fprintf(stderr, "pgmem-serve: second signal (%v) during shutdown: exiting now\n", s)
	exit(2)
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type config struct {
	socketDir   string
	port        int
	tcp         string
	ins, outs   multi
	libSuffixes multi
	sets        multi
	settings    map[string]string
	logFile     string
	maxRSSMiB   int64
	statsFile   string
	stwProbe    time.Duration
	cpuProfile  string
}

// usageText heads -h's output.
const usageText = `usage: pgmem-serve [-socket DIR] [-port N] [-tcp ADDR] [-in DIR]... [-out DIR]...
                   [-lib-suffix SUFFIX]... [-set name=value]... [-log FILE]
                   [-max-rss-mb N] [-stats FILE]

WARNING: the server uses trust authentication, so anyone who can connect
is superuser. With -tcp, that is every local user. With -out, clients can
also write files in the -out directories as the user running pgmem-serve.

`

func parseFlags(args []string, stderr io.Writer) (*config, error) {
	var c config
	fl := flag.NewFlagSet("pgmem-serve", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), usageText)
		fl.PrintDefaults()
	}
	fl.StringVar(&c.socketDir, "socket", "", "listen on the Unix socket `DIR`/.s.PGSQL.<port>; without it, pgmem-serve makes a new directory under /tmp and removes it at exit")
	fl.IntVar(&c.port, "port", 54329, "the port `N` in the socket's name")
	fl.StringVar(&c.tcp, "tcp", "", "also listen on TCP `ADDR`, a loopback address, which gives every local user superuser access")
	fl.Var(&c.ins, "in", "copy the host tree `DIR` into memfs at the same path (repeatable)")
	fl.Var(&c.outs, "out", "create `DIR` in memfs and sync it with the host directory, which clients can then write as you (repeatable)")
	fl.Var(&c.libSuffixes, "lib-suffix", "add <lib>`SUFFIX` placeholders next to each library's .so (repeatable)")
	fl.Var(&c.sets, "set", "server setting `name=value` (repeatable)")
	fl.StringVar(&c.logFile, "log", "", "write the server log to `FILE` instead of stderr")
	fl.Int64Var(&c.maxRSSMiB, "max-rss-mb", 4096, "close the server and fail once RSS passes `N` MiB")
	fl.StringVar(&c.statsFile, "stats", "", "write GC pauses, stalls and peak RSS to `FILE`")
	fl.DurationVar(&c.stwProbe, "stw-probe", 0, "for diagnosis, stop the world every `D` and log stops over 10 ms in -stats")
	fl.StringVar(&c.cpuProfile, "cpuprofile", "", "write a CPU profile of the run to `FILE`")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	if fl.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments %q", fl.Args())
	}
	if err := checkPort(c.port, "-port"); err != nil {
		return nil, err
	}
	if c.tcp != "" {
		addr, err := checkTCP(c.tcp)
		if err != nil {
			return nil, err
		}
		c.tcp = addr
	}
	for _, dirs := range []multi{c.ins, c.outs} {
		for i, d := range dirs {
			abs, err := filepath.Abs(d)
			if err != nil {
				return nil, err
			}
			if err := checkTreePath(abs); err != nil {
				return nil, err
			}
			dirs[i] = abs
		}
	}
	for _, s := range c.libSuffixes {
		if s == "" || strings.ContainsRune(s, '/') {
			return nil, fmt.Errorf("bad -lib-suffix %q", s)
		}
	}
	c.settings = map[string]string{}
	for _, s := range c.sets {
		name, val, ok := strings.Cut(s, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("bad -set %q: want name=value", s)
		}
		c.settings[name] = val
	}
	if c.maxRSSMiB <= 0 {
		return nil, fmt.Errorf("-max-rss-mb %d: must be positive", c.maxRSSMiB)
	}
	if c.stwProbe > 0 && c.statsFile == "" {
		return nil, errors.New("-stw-probe needs -stats")
	}
	return &c, nil
}

func checkPort(port int, what string) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("%s: bad port %d", what, port)
	}
	return nil
}

// checkTreePath refuses an -in or -out directory that would copy a whole
// host tree into memory, or overlay the server's own files.
func checkTreePath(dir string) error {
	if dir == "/" || dir == "/tmp" {
		return fmt.Errorf("refusing directory %s: too broad", dir)
	}
	for _, p := range []string{"/dev", "/pgmem", "/pgdata"} {
		if dir == p || strings.HasPrefix(dir, p+"/") {
			return fmt.Errorf("refusing directory %s: it is under the server's %s", dir, p)
		}
	}
	return nil
}

// checkTCP accepts a loopback IP address, or localhost as 127.0.0.1, with
// a numeric port. Port 0 picks a free one.
func checkTCP(addr string) (string, error) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("-tcp %q: %w", addr, err)
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return "", fmt.Errorf("-tcp %q: the port must be a number", addr)
	}
	if err := checkPort(port, "-tcp"); err != nil {
		return "", err
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("-tcp %q: refusing %q: not a loopback address", addr, host)
	}
	return net.JoinHostPort(host, ps), nil
}

// run is pgmem-serve, until ctx is done or the watchdog trips. It returns
// nil after a stop through ctx.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	logger := log.New(stderr, "pgmem-serve: ", log.Ltime|log.Lmicroseconds)

	sockDir := cfg.socketDir
	if sockDir == "" {
		if sockDir, err = os.MkdirTemp("/tmp", "pgmem-"); err != nil {
			return err
		}
		defer os.RemoveAll(sockDir)
	}
	sockPath := filepath.Join(sockDir, ".s.PGSQL."+strconv.Itoa(cfg.port))
	if len(sockPath) > maxSockPath {
		return fmt.Errorf("socket path %s is %d bytes, over the limit of %d: use a shorter -socket", sockPath, len(sockPath), maxSockPath)
	}

	// Defers run in reverse order. The proxy stops, then the server closes,
	// then the watchdog stops, the stats are written and the log file
	// closes.
	var logw io.Writer = stderr
	if cfg.logFile != "" {
		f, err := os.Create(cfg.logFile)
		if err != nil {
			return err
		}
		defer f.Close()
		logw = f
	}
	if cfg.cpuProfile != "" {
		f, err := os.Create(cfg.cpuProfile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	wd, err := startWatchdog(cfg.maxRSSMiB<<20, logger)
	if err != nil {
		return err
	}
	if cfg.statsFile != "" {
		st, err := startStats(cfg.statsFile, cfg.stwProbe, wd)
		if err != nil {
			wd.stop()
			return err
		}
		defer func() {
			if serr := st.finish(); serr != nil && err == nil {
				err = serr
			}
		}()
	}
	defer wd.stop()

	srv, err := startServer(ctx, wd, pgmem.Options{Settings: cfg.settings, Log: logw}, logger)
	if err != nil {
		return err
	}
	defer func() {
		t := time.Now()
		cerr := srv.Close()
		logger.Printf("server closed in %v", time.Since(t).Round(time.Millisecond))
		if cerr != nil && err == nil {
			err = cerr
		}
	}()

	fsys := serverfs.FS(srv)
	for _, d := range cfg.ins {
		n, err := copyIn(fsys, d)
		if err != nil {
			return fmt.Errorf("-in %s: %w", d, err)
		}
		logger.Printf("copied %d files from %s into memfs", n, d)
	}
	for _, d := range cfg.outs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("-out %s: %w", d, err)
		}
		if err := mkdirAll(fsys, d); err != nil {
			return fmt.Errorf("-out %s: %w", d, err)
		}
	}
	for _, suf := range cfg.libSuffixes {
		if err := addLibAliases(fsys, suf); err != nil {
			return fmt.Errorf("-lib-suffix %s: %w", suf, err)
		}
	}

	p := newProxy(srv, newOutSyncer(fsys, cfg.outs, logger), logger)
	defer p.close()
	ln, err := listenUnix(sockPath)
	if err != nil {
		return err
	}
	p.serve(ln)
	if cfg.tcp != "" {
		ln, err := net.Listen("tcp", cfg.tcp)
		if err != nil {
			return err
		}
		p.serve(ln)
	}
	if _, err := fmt.Fprintf(stdout, "ready %s\n", sockPath); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		logger.Printf("stopping")
		return nil
	case <-wd.over:
		return wd.err()
	}
}

// startServer starts the server, bounded by startTimeout, ctx and the
// watchdog.
func startServer(ctx context.Context, wd *watchdog, opts pgmem.Options, logger *log.Logger) (*pgmem.Server, error) {
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	go func() {
		select {
		case <-wd.over:
			cancel()
		case <-sctx.Done():
		}
	}()
	t := time.Now()
	srv, err := pgmem.Start(sctx, opts)
	if err != nil {
		select {
		case <-wd.over:
			return nil, wd.err()
		default:
			return nil, err
		}
	}
	logger.Printf("server started in %v", time.Since(t).Round(time.Millisecond))
	return srv, nil
}

// listenUnix listens on path, first removing a stale socket left there by
// a server that died. It refuses a live socket and any other file.
func listenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is in use", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return net.Listen("unix", path)
}
