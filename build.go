package pgmem

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/feverenergy/pgmem/internal/cluster"
)

// buildDir returns $PGMEM_BUILD_DIR, a local build from make wasm cluster
// that Start uses instead of any engine. Empty means unset. A relative
// path resolves against the process's working directory, which go test
// sets to each package's directory, so use an absolute one.
func buildDir() string { return os.Getenv("PGMEM_BUILD_DIR") }

// loadBuild returns the build Start installs and the image it boots. The
// build is postgres and share.tar, because a server never runs initdb.
// The image is image, or the build's cluster image when image is nil.
// They come from engine e, or from buildDir when it is set.
func loadBuild(e Engine, image []byte) (cluster.Build, []byte, error) {
	return loadBuildFrom(buildDir(), e, image)
}

// loadBuildFrom is loadBuild from dir, or from engine e when dir is empty.
func loadBuildFrom(dir string, e Engine, image []byte) (cluster.Build, []byte, error) {
	if dir != "" {
		return loadBuildDir(dir, image)
	}
	f, err := e.files()
	if err != nil {
		return cluster.Build{}, nil, err
	}
	var b cluster.Build
	if b.Postgres, err = f.postgres(); err != nil {
		return cluster.Build{}, nil, err
	}
	if b.Share, err = f.share(); err != nil {
		return cluster.Build{}, nil, err
	}
	if image == nil {
		if image, err = f.cluster(); err != nil {
			return cluster.Build{}, nil, err
		}
	}
	return b, image, nil
}

// buildDirLoads counts loadBuildDir calls from Start or buildID, so tests
// can tell whether an engine was used.
var buildDirLoads atomic.Int64

// loadBuildDir is loadBuild from the local build in dir. A missing file is
// an error wrapping fs.ErrNotExist. When dir is relative, the error also
// names the absolute directory it resolved to.
func loadBuildDir(dir string, image []byte) (cluster.Build, []byte, error) {
	buildDirLoads.Add(1)
	where := ""
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			where = fmt.Sprintf(" (relative: %s)", abs)
		}
	}
	read := func(name string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("pgmem: load build from PGMEM_BUILD_DIR%s: %w (run make wasm cluster there, or unset PGMEM_BUILD_DIR to use the engine)", where, err)
		}
		return data, nil
	}
	var b cluster.Build
	var err error
	if b.Postgres, err = read("postgres.wasm"); err != nil {
		return cluster.Build{}, nil, err
	}
	if b.Share, err = read("share.tar"); err != nil {
		return cluster.Build{}, nil, err
	}
	if image == nil {
		if image, err = read("cluster.tar"); err != nil {
			return cluster.Build{}, nil, err
		}
	}
	return b, image, nil
}

// buildKey is what a build ID depends on: the build directory, or, when
// that is empty, the engine's Version.
type buildKey struct{ dir, version string }

// buildIDs holds buildID's successful results. It does not keep an error,
// because a build directory that is being built may be complete on the
// next call.
var buildIDs = struct {
	sync.Mutex
	m map[buildKey]string
}{m: map[buildKey]string{}}

// buildID is buildid.ID, for engine e. It returns the lowercase hex
// sha256 over the engine's Version, postgres.wasm, share.tar and
// cluster.tar, in that order, each preceded by its length as 8 big-endian
// bytes. It hashes the bytes Start would use, loaded as Start loads them.
// When buildDir is set, those are the directory's files for every engine,
// and the Version hashed is empty. Otherwise e must be an engine Start
// accepts. It hashes once per process for each Version, or each value of
// buildDir, and keeps only successes. Callers wait for each other, so a
// build is never hashed twice at once.
func buildID(e Engine) (string, error) {
	e = e.resolve()
	key := buildKey{dir: buildDir()}
	if key.dir == "" {
		// Check e as Start does, so that a cached ID is never returned
		// for an Engine that Start would refuse.
		if _, err := e.files(); err != nil {
			return "", err
		}
		key.version = e.Version
	}
	buildIDs.Lock()
	defer buildIDs.Unlock()
	if id, ok := buildIDs.m[key]; ok {
		return id, nil
	}
	id, err := hashBuild(key, e)
	if err != nil {
		return "", err
	}
	buildIDs.m[key] = id
	return id, nil
}

// hashBuild computes the build ID for key: of the build in key.dir, or of
// engine e when key.dir is empty.
func hashBuild(key buildKey, e Engine) (string, error) {
	b, image, err := loadBuildFrom(key.dir, e, nil)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, data := range [][]byte{[]byte(key.version), b.Postgres, b.Share, image} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(data))))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
