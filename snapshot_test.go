package pgmem_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/feverenergy/pgmem"
	"github.com/feverenergy/pgmem/internal/buildid"
)

func countRows(t *testing.T, s *pgmem.Server, table string) int {
	t.Helper()
	c := connect(t, s)
	defer c.Close(testCtx(t))
	var n int
	if err := c.QueryRow(testCtx(t), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSnapshotBoots(t *testing.T) {
	s := start(t, pgmem.Options{})
	c := connect(t, s)
	ctx := testCtx(t)
	if _, err := c.Exec(ctx, "CREATE TABLE t AS SELECT g AS id FROM generate_series(1,1000) g"); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	t0 := time.Now()
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot: %d bytes in %v", len(snap), time.Since(t0))
	s.Close()
	s2 := start(t, pgmem.Options{Image: snap})
	if n := countRows(t, s2, "t"); n != 1000 {
		t.Fatalf("rows = %d, want 1000", n)
	}
}

// TestSnapshotUnderLoad checks that every commit made before Snapshot is
// called is in the image, with synchronous_commit on or off. CHECKPOINT
// flushes the WAL. Workers 0 and 2 commit synchronously, and 1 and 3 do
// not.
func TestSnapshotUnderLoad(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	setup := connect(t, s)
	if _, err := setup.Exec(ctx, "CREATE TABLE l (w int, id int)"); err != nil {
		t.Fatal(err)
	}
	setup.Close(ctx)

	const workers = 4
	var committed [workers]atomic.Int64 // highest committed id per worker
	var before [workers]int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// This does not use connect, because t.Fatal must not run
			// outside the test goroutine.
			c, err := pgx.ConnectConfig(ctx, connConfig(t, s))
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close(context.Background())
			mode := "on"
			if w%2 == 1 {
				mode = "off"
			}
			if _, err := c.Exec(ctx, "SET synchronous_commit = "+mode); err != nil {
				t.Error(err)
				return
			}
			for id := int64(1); ; id++ {
				select {
				case <-stop:
					return
				default:
				}
				tx, err := c.Begin(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := tx.Exec(ctx, "INSERT INTO l VALUES ($1,$2)", w, id); err != nil {
					t.Error(err)
					return
				}
				if err := tx.Commit(ctx); err != nil {
					t.Error(err)
					return
				}
				committed[w].Store(id)
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	for w := range before {
		before[w] = committed[w].Load()
	}
	t0 := time.Now()
	snap, err := s.Snapshot(ctx)
	elapsed := time.Since(t0)
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot under load: %d bytes in %v; committed before it: %v", len(snap), elapsed, before)
	if cap(snap) != len(snap) {
		t.Errorf("image cap %d, len %d: want them equal", cap(snap), len(snap))
	}
	for w, n := range before {
		if n == 0 {
			t.Fatalf("worker %d committed nothing in 500 ms before the snapshot", w)
		}
	}
	s.Close()

	s2 := start(t, pgmem.Options{Image: snap})
	c := connect(t, s2)
	defer c.Close(ctx)
	for w := range before {
		var n int64
		if err := c.QueryRow(ctx, "SELECT count(*) FROM l WHERE w=$1 AND id<=$2", w, before[w]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != before[w] {
			t.Errorf("worker %d: %d of %d rows committed before the snapshot", w, n, before[w])
		}
	}
}

// TestSnapshotDropsUnloggedData pins what an image booted through crash
// recovery keeps. It keeps logged tables. It does not keep the contents of
// unlogged tables, unlogged sequences' state or cumulative statistics.
func TestSnapshotDropsUnloggedData(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "CREATE TABLE logged AS SELECT g AS id FROM generate_series(1, 100) g")
	exec(t, ctx, c, "CREATE UNLOGGED TABLE unlogged AS SELECT g AS id FROM generate_series(1, 100) g")
	exec(t, ctx, c, "CREATE UNLOGGED SEQUENCE useq")
	exec(t, ctx, c, "SELECT nextval('useq') FROM generate_series(1, 5)")
	exec(t, ctx, c, "INSERT INTO logged VALUES (101)")
	exec(t, ctx, c, "SELECT pg_stat_force_next_flush()")
	var ins int64
	if err := c.QueryRow(ctx, "SELECT n_tup_ins FROM pg_stat_user_tables WHERE relname = 'logged'").Scan(&ins); err != nil || ins == 0 {
		t.Fatalf("n_tup_ins before the snapshot: %d %v; want it counted", ins, err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := start(t, pgmem.Options{Image: snap})
	if n := countRows(t, s2, "logged"); n != 101 {
		t.Errorf("logged table: %d rows, want 101", n)
	}
	if n := countRows(t, s2, "unlogged"); n != 0 {
		t.Errorf("unlogged table: %d rows, want 0 (crash recovery resets unlogged relations)", n)
	}
	c2 := connect(t, s2)
	var next int64
	if err := c2.QueryRow(ctx, "SELECT nextval('useq')").Scan(&next); err != nil || next != 1 {
		t.Errorf("unlogged sequence: nextval %d %v, want 1", next, err)
	}
	if err := c2.QueryRow(ctx, "SELECT n_tup_ins FROM pg_stat_user_tables WHERE relname = 'logged'").Scan(&ins); err != nil || ins != 0 {
		t.Errorf("n_tup_ins after boot: %d %v, want 0 (statistics reset)", ins, err)
	}
}

// TestSnapshotRacingClose checks that a Snapshot that Close interrupts
// fails with an error wrapping net.ErrClosed, as one after Close does. A
// session holding pg_database locked keeps the snapshot's own session
// waiting in its startup until Close ends both.
func TestSnapshotRacingClose(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "BEGIN")
	exec(t, ctx, c, "LOCK TABLE pg_database IN ACCESS EXCLUSIVE MODE")
	snap := async(func() error { _, err := s.Snapshot(ctx); return err })
	for waiting := 0; waiting == 0; {
		if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-snap:
			t.Fatalf("snapshot ended before Close: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	s.Close()
	err := <-snap
	t.Logf("snapshot during Close: %v", err)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want an error wrapping net.ErrClosed", err)
	}
}

func TestSnapshotAfterClose(t *testing.T) {
	s := start(t, pgmem.Options{})
	s.Close()
	if _, err := s.Snapshot(testCtx(t)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed", err)
	}
}

// shutdownImage stops s with Shutdown and returns its image, failing the
// test if there is none.
func shutdownImage(t *testing.T, s *pgmem.Server) []byte {
	t.Helper()
	t0 := time.Now()
	img, err := s.Shutdown(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("shutdown image: %d bytes in %v", len(img), time.Since(t0))
	if len(img) == 0 {
		t.Fatal("Shutdown returned an empty image")
	}
	return img
}

// tarNames lists the entry names of a tar image.
func tarNames(t *testing.T, img []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(img))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

const notShutDown = "database system was not properly shut down"

// TestShutdownKeepsUnloggedData pins what a clean-shutdown image keeps, as
// TestSnapshotDropsUnloggedData pins what a Snapshot drops. The image has
// no postmaster.pid, and a server started from it skips crash recovery.
// Unlogged tables, unlogged sequences and cumulative statistics survive.
func TestShutdownKeepsUnloggedData(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	c := connect(t, s)
	exec(t, ctx, c, "CREATE TABLE logged (id int)")
	exec(t, ctx, c, "INSERT INTO logged VALUES (1)")
	exec(t, ctx, c, "CREATE UNLOGGED TABLE unlogged (id int)")
	exec(t, ctx, c, "INSERT INTO unlogged VALUES (2)")
	exec(t, ctx, c, "CREATE UNLOGGED SEQUENCE useq")
	exec(t, ctx, c, "SELECT nextval('useq') FROM generate_series(1, 5)")
	exec(t, ctx, c, "SELECT pg_stat_force_next_flush()")
	img := shutdownImage(t, s) // with c still open, which fast shutdown ends
	for _, name := range tarNames(t, img) {
		if path.Base(name) == "postmaster.pid" {
			t.Errorf("image holds %s", name)
		}
	}

	s2 := start(t, pgmem.Options{Image: img})
	c2 := connect(t, s2)
	var logged, unlogged int
	if err := c2.QueryRow(ctx, "SELECT (SELECT id FROM logged), (SELECT id FROM unlogged)").Scan(&logged, &unlogged); err != nil {
		t.Fatal(err)
	}
	if logged != 1 || unlogged != 2 {
		t.Errorf("rows: logged %d, unlogged %d; want 1 and 2", logged, unlogged)
	}
	var next int64
	if err := c2.QueryRow(ctx, "SELECT nextval('useq')").Scan(&next); err != nil || next != 6 {
		t.Errorf("unlogged sequence: nextval %d %v, want 6", next, err)
	}
	var ins int64
	if err := c2.QueryRow(ctx, "SELECT n_tup_ins FROM pg_stat_user_tables WHERE relname = 'logged'").Scan(&ins); err != nil || ins != 1 {
		t.Errorf("n_tup_ins after boot: %d %v, want 1 (statistics kept)", ins, err)
	}
	if log := s2.Log(); strings.Contains(log, notShutDown) {
		t.Errorf("the server started from the image ran crash recovery:\n%s", log)
	}
}

func TestShutdownAfterClose(t *testing.T) {
	s := start(t, pgmem.Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := s.Shutdown(testCtx(t))
	if !errors.Is(err, net.ErrClosed) || img != nil {
		t.Fatalf("Shutdown after Close: %d bytes, %v; want no image and net.ErrClosed", len(img), err)
	}
}

// TestShutdownThenClose checks that the server is closed after Shutdown.
// Close returns nil. A second Shutdown, Dial and Snapshot fail with
// net.ErrClosed. No process is left, and start's cleanup checks that too.
func TestShutdownThenClose(t *testing.T) {
	s := start(t, pgmem.Options{})
	shutdownImage(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close after Shutdown: %v", err)
	}
	ctx := testCtx(t)
	if img, err := s.Shutdown(ctx); !errors.Is(err, net.ErrClosed) || img != nil {
		t.Errorf("second Shutdown: %d bytes, %v; want no image and net.ErrClosed", len(img), err)
	}
	if _, err := s.Dial(ctx, "unix", ""); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Dial after Shutdown: %v, want net.ErrClosed", err)
	}
	if _, err := s.Snapshot(ctx); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Snapshot after Shutdown: %v, want net.ErrClosed", err)
	}
	if n := pgmem.Kernel(s).ProcCount(); n != 0 {
		t.Errorf("%d processes left after Shutdown", n)
	}
}

// TestShutdownEscalates checks the case where fast shutdown does not
// finish within fastShutdownWait or before ctx is done. Shutdown then
// escalates as Close does and returns an error quoting the log, with no
// image.
//
// The escalation line names its cause, which is the wait timing out or
// ctx.
func TestShutdownEscalates(t *testing.T) {
	check := func(t *testing.T, s *pgmem.Server, img []byte, err error, want error, quitLine string) {
		t.Helper()
		t.Logf("Shutdown: %v", err)
		if err == nil || img != nil {
			t.Fatalf("Shutdown: %d bytes, %v; want no image and an error", len(img), err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("err = %v, want it to wrap %v", err, want)
		}
		if !strings.Contains(err.Error(), "last log lines:") {
			t.Errorf("err does not quote the log: %v", err)
		}
		if !strings.Contains(s.Log(), quitLine) {
			t.Errorf("log lacks %q:\n%s", quitLine, s.Log())
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close after a failed Shutdown: %v", err)
		}
	}
	t.Run("wait", func(t *testing.T) {
		defer pgmem.SetCloseWaits(time.Nanosecond, scaled(5*time.Second), scaled(5*time.Second), nil)()
		s := start(t, pgmem.Options{})
		connect(t, s)
		img, err := s.Shutdown(testCtx(t))
		check(t, s, img, err, nil, "pgmem: fast shutdown timed out after 1ns; sent SIGQUIT")
	})
	t.Run("ctx", func(t *testing.T) {
		s := start(t, pgmem.Options{})
		connect(t, s)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		img, err := s.Shutdown(ctx)
		check(t, s, img, err, context.Canceled, "pgmem: fast shutdown cut short (context canceled); sent SIGQUIT")
	})
}

// TestShutdownBootTime logs how long a server takes to start from a
// clean-shutdown image and from a Snapshot of the same cluster, which boots
// through crash recovery. It only measures, and does not assert on the
// times.
func TestShutdownBootTime(t *testing.T) {
	s := start(t, pgmem.Options{})
	ctx := testCtx(t)
	exec(t, ctx, connect(t, s), "CREATE TABLE t AS SELECT g AS id FROM generate_series(1, 1000) g")
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clean := shutdownImage(t, s)

	// boot starts and closes a server from img, without start's cleanup,
	// which would keep each server's file system until the test ends.
	boot := func(img []byte) (time.Duration, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), scaled(60*time.Second))
		defer cancel()
		t0 := time.Now()
		s, err := pgmem.Start(ctx, pgmem.Options{Image: img})
		elapsed := time.Since(t0)
		if notBuilt(err) {
			t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
		}
		if err != nil {
			t.Fatal(err)
		}
		log := s.Log()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if n := pgmem.Kernel(s).ProcCount(); n != 0 {
			t.Fatalf("%d processes left after Close", n)
		}
		return elapsed, log
	}
	const rounds = 3
	var fromSnap, fromClean []time.Duration
	for range rounds {
		d, log := boot(snap)
		if !strings.Contains(log, notShutDown) {
			t.Fatalf("the server started from the Snapshot did not run crash recovery:\n%s", log)
		}
		fromSnap = append(fromSnap, d)
		d, log = boot(clean)
		if strings.Contains(log, notShutDown) {
			t.Fatalf("the server started from the clean image ran crash recovery:\n%s", log)
		}
		fromClean = append(fromClean, d)
	}
	t.Logf("boot from Snapshot (%d bytes): %v, min %v", len(snap), fromSnap, slices.Min(fromSnap))
	t.Logf("boot from clean image (%d bytes): %v, min %v", len(clean), fromClean, slices.Min(fromClean))
}

// TestBuildIDStable checks that the build ID is stable within a process
// and is lowercase hex sha256. It hashes the engine's Version,
// postgres.wasm, share.tar and cluster.tar in that order, each preceded by
// its length as 8 big-endian bytes. With PGMEM_BUILD_DIR set, it hashes
// that directory's files and an empty Version.
func TestBuildIDStable(t *testing.T) {
	id1, err := buildid.ID("", nil)
	if notBuilt(err) {
		t.Skip("PGMEM_BUILD_DIR is not built; run make wasm cluster")
	}
	if err != nil {
		t.Fatal(err)
	}
	id2, err := buildid.ID("", nil)
	if err != nil || id2 != id1 {
		t.Fatalf("second call: %q %v, want %q", id2, err, id1)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id1) {
		t.Fatalf("build ID %q is not 64 lowercase hex characters", id1)
	}
	t.Logf("build ID: %s", id1)

	files := []string{"postgres.wasm", "share.tar", "cluster.tar"}
	idOf := func(contents ...string) string {
		t.Helper()
		dir := t.TempDir()
		h := sha256.New()
		h.Write(binary.BigEndian.AppendUint64(nil, 0)) // the empty Version
		for i, name := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(contents[i]), 0o600); err != nil {
				t.Fatal(err)
			}
			h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(contents[i]))))
			h.Write([]byte(contents[i]))
		}
		t.Setenv("PGMEM_BUILD_DIR", dir)
		id, err := buildid.ID("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := hex.EncodeToString(h.Sum(nil)); id != want {
			t.Fatalf("build ID of %q: %s, want %s", contents, id, want)
		}
		return id
	}
	a := idOf("ab", "c", "d")
	b := idOf("a", "bc", "d") // the same bytes, split differently
	c := idOf("ab", "c", "e")
	if a == id1 || a == b || a == c || b == c {
		t.Fatalf("build IDs do not depend on the bytes: %s %s %s %s", id1, a, b, c)
	}
	t.Setenv("PGMEM_BUILD_DIR", t.TempDir()) // empty, so a missing build
	if _, err := buildid.ID("", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("build ID of an empty build directory: %v, want fs.ErrNotExist", err)
	}
}

// TestBuildIDErrorNotKept checks that an error is not remembered. A build
// directory that is missing its files, then gets them, has an ID on the
// next call.
func TestBuildIDErrorNotKept(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PGMEM_BUILD_DIR", dir)
	if _, err := buildid.ID("", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("build ID of an empty build directory: %v, want fs.ErrNotExist", err)
	}
	for _, name := range []string{"postgres.wasm", "share.tar", "cluster.tar"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id, err := buildid.ID("", nil)
	if err != nil {
		t.Fatalf("build ID once the files exist: %v", err)
	}
	if again, err := buildid.ID("", nil); err != nil || again != id {
		t.Fatalf("second call: %q %v, want %q", again, err, id)
	}
}
