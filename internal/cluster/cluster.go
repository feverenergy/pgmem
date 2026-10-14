// Package cluster makes Postgres clusters inside pgmem. It installs a
// build into a memfs, runs initdb, bakes in the test settings, and runs
// single-user SQL.
package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/feverenergy/pgmem/internal/kernel"
	"github.com/feverenergy/pgmem/internal/memfs"
)

// DataDir is where a cluster lives in the memfs.
const DataDir = "/pgdata"

// The directory share.tar loads under, and the program paths Install
// registers. PostgresPath is the server's.
const (
	installDir   = "/pgmem"
	PostgresPath = installDir + "/bin/postgres"
	initdbPath   = installDir + "/bin/initdb"
)

// Settings are appended to postgresql.conf of every new cluster. The
// settings are described in docs/architecture.md.
var Settings = []string{
	"fsync = off",
	"synchronous_commit = off",
	"full_page_writes = off",
	"autovacuum = off",
	"shared_buffers = 32MB",
	"timezone = 'UTC'",
	"max_connections = 200",
	"dynamic_shared_memory_type = sysv",
	"shared_memory_type = sysv",
	"min_dynamic_shared_memory = 32MB",
	"max_worker_processes = 2",
	"max_parallel_workers = 0",
	"max_logical_replication_workers = 0",
	"wal_level = minimal",
	"max_wal_senders = 0",
	// pg_wal and temporary files live in the memfs, which is RAM. A
	// checkpoint starts once WAL reaches max_wal_size, and recycling keeps
	// between min_wal_size and max_wal_size. temp_file_limit fails a query
	// that would spill more.
	"max_wal_size = '64MB'",
	"min_wal_size = '32MB'",
	"temp_file_limit = '1GB'",
	"log_line_prefix = '%m [%p] %d '",
	"listen_addresses = ''",
	"unix_socket_directories = '/tmp'",
}

// HBA is the pg_hba.conf of every new cluster. It allows Unix-socket
// connections only. initdb's "host ... 127.0.0.1/32" lines make the
// postmaster exit, because pgmem has no name resolution. compat's
// getaddrinfo always fails, and pgmem has no TCP anyway.
const HBA = "local all all trust\nlocal replication all trust\n"

// Build is the output of make wasm. Initdb may be nil when the build only
// runs servers. See Install.
type Build struct {
	Postgres, Initdb, Share []byte
}

// LoadBuild reads postgres.wasm, initdb.wasm and share.tar from dir. A
// missing file is an error wrapping fs.ErrNotExist.
func LoadBuild(dir string) (Build, error) {
	var b Build
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{
		{"postgres.wasm", &b.Postgres},
		{"initdb.wasm", &b.Initdb},
		{"share.tar", &b.Share},
	} {
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return Build{}, fmt.Errorf("cluster: load build: %w", err)
		}
		*f.dst = data
	}
	return b, nil
}

// Install loads b.Share under /pgmem and writes placeholder program files
// at /pgmem/bin/postgres and /pgmem/bin/initdb, because Postgres checks
// that its own executable exists. It then compiles and registers both
// programs. A server needs only postgres, so b.Initdb may be nil. Install
// then skips initdb, with no placeholder and no compile.
func Install(ctx context.Context, k *kernel.Kernel, fs *memfs.FS, b Build) error {
	if err := fs.LoadTar(bytes.NewReader(b.Share), installDir); err != nil {
		return fmt.Errorf("cluster: install share.tar: %w", err)
	}
	for _, p := range []struct {
		path string
		wasm []byte
	}{
		{PostgresPath, b.Postgres},
		{initdbPath, b.Initdb},
	} {
		if p.wasm == nil && p.path == initdbPath {
			continue
		}
		if err := fs.WriteFile(p.path, []byte("pgmem\n")); err != nil {
			return fmt.Errorf("cluster: write %s: %w", p.path, err)
		}
		m, err := k.Compile(ctx, p.wasm)
		if err != nil {
			return fmt.Errorf("cluster: compile %s: %w", p.path, err)
		}
		if err := k.Register(p.path, m); err != nil {
			return fmt.Errorf("cluster: %w", err)
		}
	}
	return nil
}

// Create runs initdb into DataDir, appends Settings to its postgresql.conf
// and replaces its pg_hba.conf with HBA. initdb's output goes to log, and
// a nil log discards it. If initdb fails, the error quotes the last 20
// lines it printed.
//
// ctx only gates starting initdb. Once initdb runs, nothing on the Go side
// kills it, and an initdb that never exits ends only when the kernel is
// closed.
func Create(ctx context.Context, k *kernel.Kernel, fs *memfs.FS, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	out := &tailWriter{w: log, max: 20}
	p, err := k.Start(ctx, kernel.Spec{
		Path: initdbPath,
		Args: []string{"-D", DataDir, "-U", "postgres", "--auth=trust", "-E", "UTF8", "--locale=C", "--no-sync", "--wal-segsize=1"},
		Env:  []string{"TZ=UTC"},
		// Both streams share out, so its lines interleave as printed.
		Stdout: out,
		Stderr: out,
	})
	if err != nil {
		return fmt.Errorf("cluster: initdb: %w", err)
	}
	if st := p.Wait(); !st.Exited() || st.ExitCode() != 0 {
		return fmt.Errorf("cluster: initdb failed (status %#x); last output:\n%s", int32(st), out.tail())
	}

	conf := DataDir + "/postgresql.conf"
	data, err := fs.ReadFile(conf)
	if err != nil {
		return fmt.Errorf("cluster: read %s: %w", conf, err)
	}
	var sb strings.Builder
	sb.Write(data)
	sb.WriteString("\n# pgmem test settings\n")
	for _, s := range Settings {
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	if err := fs.WriteFile(conf, []byte(sb.String())); err != nil {
		return fmt.Errorf("cluster: write %s: %w", conf, err)
	}
	hba := DataDir + "/pg_hba.conf"
	if err := fs.WriteFile(hba, []byte(HBA)); err != nil {
		return fmt.Errorf("cluster: write %s: %w", hba, err)
	}
	return nil
}

// Single runs "postgres --single -D DataDir postgres" with sql on stdin and
// returns what it printed. Each statement must fit on one line.
//
// ctx only gates starting postgres. Once it runs, nothing on the Go side
// kills it, and a process that never exits ends only when the kernel is
// closed.
func Single(ctx context.Context, k *kernel.Kernel, sql string) (stdout, stderr string, st kernel.Status, err error) {
	var out, errOut bytes.Buffer
	p, err := k.Start(ctx, kernel.Spec{
		Path:   PostgresPath,
		Args:   []string{"--single", "-D", DataDir, "postgres"},
		Stdin:  strings.NewReader(sql),
		Stdout: &out,
		Stderr: &errOut,
	})
	if err != nil {
		return "", "", 0, fmt.Errorf("cluster: postgres --single: %w", err)
	}
	st = p.Wait()
	return out.String(), errOut.String(), st, nil
}

// maxPartial bounds how much of the end of an unterminated line tailWriter
// keeps, so guest output cannot grow it without limit.
const maxPartial = 4 << 10

// tailWriter passes writes to w and keeps the last max lines for an error
// message. It keeps complete lines plus the end of any unterminated one,
// and cuts each line to its last maxPartial bytes.
type tailWriter struct {
	mu      sync.Mutex
	w       io.Writer
	max     int
	lines   []string
	partial []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			t.addPartial(rest)
			break
		}
		t.addPartial(rest[:i])
		t.lines = append(t.lines, string(t.partial))
		t.partial = t.partial[:0]
		if len(t.lines) > t.max {
			t.lines = append(t.lines[:0], t.lines[1:]...)
		}
		rest = rest[i+1:]
	}
	return t.w.Write(p)
}

// addPartial appends b to the unterminated line, keeping its last
// maxPartial bytes.
func (t *tailWriter) addPartial(b []byte) {
	if len(b) >= maxPartial {
		t.partial = append(t.partial[:0], b[len(b)-maxPartial:]...)
		return
	}
	if over := len(t.partial) + len(b) - maxPartial; over > 0 {
		t.partial = t.partial[:copy(t.partial, t.partial[over:])]
	}
	t.partial = append(t.partial, b...)
}

// tail returns the last max lines, counting an unterminated last line,
// each newline-terminated.
func (t *tailWriter) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := t.lines
	if len(t.partial) > 0 && len(lines) == t.max {
		lines = lines[1:]
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	if len(t.partial) > 0 {
		sb.Write(t.partial)
		sb.WriteByte('\n')
	}
	return sb.String()
}
