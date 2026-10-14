package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/feverenergy/pgmem/internal/buildid"
)

// The snapshot cache keeps each template's image in a file. The file name
// covers the Postgres build (its build ID, which names the engine), what
// pgtest adds to the image and the template's Key (see cacheFileName). A
// test binary therefore builds a template only when no binary has built
// it before. Builders serialize on a lock file next to the image. The
// first one builds the image and renames it into place. The others wait
// on the lock, then read the file. A build that fails leaves a failure
// marker next to the image, so the processes that waited for it report
// its error instead of each building again. This file does not validate
// the image. The caller boots it, and if that fails, calls discardCached
// and templateImage once more. Each write of a new image prunes the
// images not used for 30 days.

// pruneAge is how long an image may go unused before a new image's write
// removes it. Each cache hit sets the image's modification time.
const pruneAge = 30 * 24 * time.Hour

// templateImage returns the image for tpl. That is the cached file if there
// is one, and otherwise a new image, which it builds and caches. ctx bounds
// neither the build nor the wait on the lock. Every process waiting for a
// build shares it, so the build runs under its own 10-minute timeout (see
// buildTemplate).
//
// logf receives one line when templateImage builds an image. If the cache
// cannot be used, logf receives one line saying so, and templateImage
// builds the image in memory. The causes are no user cache directory, a
// relative PGTEST_CACHE_DIR, a directory or lock file that cannot be
// created, or an image that cannot be written. That warning also goes to
// stderr, once per process. A cache problem never fails the build. A file
// system without flock support (ENOLCK, ENOTSUP) works too. templateImage
// then builds the image and writes it atomically without the lock, so at
// worst two processes build it.
//
// If another process's build of the same image failed while this process
// waited for the lock, templateImage returns that process's error instead
// of running Migrate again.
func templateImage(ctx context.Context, tpl Template, logf func(string, ...any)) ([]byte, error) {
	dir, err := cacheDir()
	if err != nil {
		where := dir // a relative PGTEST_CACHE_DIR
		if where == "" {
			where = "(no user cache directory)"
		}
		return buildUncached(tpl, where, err, logf)
	}
	if dir == "" { // PGTEST_CACHE_DIR=off
		start := time.Now()
		img, err := buildTemplate(tpl)
		if err != nil {
			return nil, err
		}
		logf("pgtest: built template %q in %v (%s); snapshot cache off", tpl.Key, time.Since(start).Round(time.Millisecond), sizeMB(img))
		return img, nil
	}
	path, err := cachePath(dir, tpl)
	if err != nil {
		return buildUncached(tpl, dir, err, logf)
	}
	if img, ok := readCached(path); ok {
		return img, nil
	}

	// Build it, unless another process built it while this one waited on
	// the lock.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return buildUncached(tpl, dir, err, logf)
	}
	noLock := ""               // why the build runs without the lock, if it does
	marker := path + ".failed" // the failure marker
	before, _ := os.Stat(marker)
	lock, err := lockFile(path + ".lock")
	switch {
	case err == nil:
		defer unlockFile(lock)
		now := time.Now() // a lock file in use is never pruned
		os.Chtimes(path+".lock", now, now)
	case lockUnsupported(err):
		noLock = fmt.Sprintf(" (without the lock: %v)", err)
	default:
		return buildUncached(tpl, dir, err, logf)
	}
	img, err := os.ReadFile(path)
	if err == nil {
		touch(path)
		return img, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return buildUncached(tpl, dir, err, logf)
	}
	if noLock == "" {
		if text, ok := failedWhileWaiting(marker, before); ok {
			return nil, fmt.Errorf("pgtest: template build failed in another process: %s", text)
		}
	}
	start := time.Now()
	img, err = buildTemplate(tpl)
	if err != nil {
		if noLock == "" {
			writeFileAtomic(marker, []byte(err.Error())) // for the processes waiting
		}
		return nil, err
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if noLock == "" {
		removeStaleTemp(path)
		os.Remove(marker)
	}
	if err := writeFileAtomic(path, img); err != nil {
		warnCache(logf, unusableFormat, dir, err)
		return img, nil
	}
	pruneCache(dir, path)
	logf("pgtest: built template %q in %v (%s); cached as %s%s", tpl.Key, elapsed, sizeMB(img), path, noLock)
	return img, nil
}

// readCached returns the image at path, if it can be read, and sets its
// modification time (see pruneCache).
func readCached(path string) ([]byte, bool) {
	img, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	touch(path)
	return img, true
}

// touch sets path's modification time to now, if it can.
func touch(path string) {
	now := time.Now()
	os.Chtimes(path, now, now)
}

// failedWhileWaiting returns the text of the failure marker at path if the
// marker appeared or changed while the caller waited on the lock. That
// means a build failed in the meantime. before is the marker's os.Stat
// from before the wait, or nil if there was none. It compares the file
// with its earlier state instead of comparing its modification time with
// the clock, so a wrong clock or a file system's coarse timestamps do not
// matter. A builder writes the marker to a new file and renames it into
// place, so a new marker is a different file.
func failedWhileWaiting(path string, before fs.FileInfo) (string, bool) {
	after, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if before != nil && os.SameFile(before, after) && before.ModTime().Equal(after.ModTime()) && before.Size() == after.Size() {
		return "", false // a stale marker, from a failure before the wait
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(text), true
}

// cacheName matches the names pruneCache may remove: an image's
// (cacheFileName), its lock file's and failure marker's, and the
// temporary files that writeFileAtomic writes for the image and the
// marker. The groups are the image's name and the suffix.
var cacheName = regexp.MustCompile(`^([0-9a-f]{16}-[0-9a-f]{16}\.img)(|\.lock|\.failed|(?:\.failed)?\.tmp-[0-9]+)$`)

// pruneCache removes from dir the images whose modification time is more
// than pruneAge ago, with their lock files and failure markers. It also
// removes lock files and failure markers that old whose image does not
// exist, and temporary files that old, which a builder left when it died
// before its rename (see writeFileAtomic). keep, the image just written,
// stays. It removes only pgtest's names (cacheName), so a cache directory
// may hold other files. It ignores errors, because pruning is only
// housekeeping. It takes no lock. An image unused for 30 days has no
// reader, a lock file in use has a recent modification time (templateImage
// sets it), and a temporary file lives only as long as one write.
func pruneCache(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-pruneAge)
	old := func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.ModTime().Before(cutoff)
	}
	for _, e := range entries {
		name := e.Name()
		m := cacheName.FindStringSubmatch(name)
		if m == nil || !old(name) {
			continue
		}
		img, suffix := m[1], m[2]
		switch {
		case strings.Contains(suffix, ".tmp-"): // orphaned
			os.Remove(filepath.Join(dir, name))
		case filepath.Join(dir, img) == keep:
		case suffix == "":
			for _, f := range []string{name, name + ".lock", name + ".failed"} {
				os.Remove(filepath.Join(dir, f))
			}
		default: // a lock file or failure marker, removed when its image is gone
			if _, err := os.Lstat(filepath.Join(dir, img)); errors.Is(err, fs.ErrNotExist) {
				os.Remove(filepath.Join(dir, name))
			}
		}
	}
}

// cacheStderr receives the process's cache warning; cacheWarned records
// that it was written. Tests replace and reset them.
var (
	cacheStderr io.Writer = os.Stderr
	cacheWarned atomic.Bool
)

// warnCache sends a cache warning to logf. The first time in the process,
// it also writes the warning to stderr. go test shows a passing test's
// t.Logf only with -v, and a cache that never works costs a build per
// binary.
func warnCache(logf func(string, ...any), format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logf("%s", msg)
	if cacheWarned.CompareAndSwap(false, true) {
		fmt.Fprintln(cacheStderr, msg)
	}
}

const unusableFormat = "pgtest: snapshot cache %s unusable (%v); building in memory"

// buildUncached warns that the cache in dir cannot be used, because of
// err, and builds tpl's image without it.
func buildUncached(tpl Template, dir string, err error, logf func(string, ...any)) ([]byte, error) {
	warnCache(logf, unusableFormat, dir, err)
	return buildTemplate(tpl)
}

// removeStaleTemp removes the temporary files for path (see
// writeFileAtomic) that a builder left when it was killed before its
// rename. Its caller holds the lock, so no other builder is writing one.
func removeStaleTemp(path string) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".tmp-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// discardCached deletes tpl's cached image, under the lock, so that the
// next templateImage builds it again. Its caller found the image unusable.
// A missing cache or image is not an error.
func discardCached(tpl Template) error {
	dir, err := cacheDir()
	if err != nil || dir == "" {
		return err
	}
	path, err := cachePath(dir, tpl)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	lock, err := lockFile(path + ".lock")
	switch {
	case err == nil:
		defer unlockFile(lock)
	case lockUnsupported(err): // templateImage writes it without the lock, so remove it without one too
	default:
		return fmt.Errorf("pgtest: discard cached image: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pgtest: discard cached image: %w", err)
	}
	return nil
}

// cacheDir returns the snapshot cache's directory: $PGTEST_CACHE_DIR, or
// os.UserCacheDir()/pgmem/snapshots when that is empty. It returns "" when
// $PGTEST_CACHE_DIR is "off". A relative $PGTEST_CACHE_DIR is an error,
// and cacheDir returns the path with it. go test runs each package's
// binary in the package's directory, so each package would get a cache of
// its own.
func cacheDir() (string, error) {
	switch dir := os.Getenv("PGTEST_CACHE_DIR"); dir {
	case "off":
		return "", nil
	case "":
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "pgmem", "snapshots"), nil
	default:
		if !filepath.IsAbs(dir) {
			return dir, fmt.Errorf("PGTEST_CACHE_DIR=%q is not an absolute path", dir)
		}
		return dir, nil
	}
}

// cachePath is the file in dir that holds tpl's image (see cacheFileName).
func cachePath(dir string, tpl Template) (string, error) {
	id, err := buildid.ID(tpl.Engine.Version, tpl.Engine.FS)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheFileName(id, supportSQL, templateLayoutVersion, tpl.Key)), nil
}

// cacheFileName is the name of the image file for build id, support schema
// support, template layout version layout and template Key key:
//
//	<id[:16]>-<hex(sha256(sha256(support) || 0x00 || layout || 0x00 || key))[:16]>.img
//
// It covers everything that goes into the image: the Postgres build, what
// pgtest adds (support.sql and how buildTemplate builds), and the Key,
// which is hashed because it may hold any characters.
func cacheFileName(id, support, layout, key string) string {
	s := sha256.Sum256([]byte(support))
	h := sha256.New()
	h.Write(s[:])
	h.Write([]byte{0})
	h.Write([]byte(layout))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return id[:16] + "-" + hex.EncodeToString(h.Sum(nil))[:16] + ".img"
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, synced and then renamed into place, so that no reader ever
// sees part of it.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sizeMB formats the size of img in MB.
func sizeMB(img []byte) string {
	return fmt.Sprintf("%.1f MB", float64(len(img))/(1<<20))
}
