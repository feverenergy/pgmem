// mkassets compresses the output of make wasm cluster into an engine
// directory, such as pgmem-engine/pg17. It writes one zstd file per input,
// a SHA256SUMS of them, an INPUTS.SHA256SUMS of the uncompressed inputs,
// and a PROVENANCE. PROVENANCE lists pins from build/versions.env, the
// sha256 of each patch, and a sha256 of the source tree the build reads
// (see sourceTree). INPUTS.SHA256SUMS lets make assets-check tell a
// content change from an encoding change. The encoder settings are
// fixed, so the same inputs give the same bytes on every run and every OS.
// make assets-check relies on that.
//
//	go run ./internal/cmd/mkassets -in build/out -o ../pgmem-engine/pg17
//
// It reads the sources from the current directory, which must be the
// root of the pgmem repository, or from -root.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// inputs are the files of make wasm cluster that an engine holds.
// initdb.wasm is not among them, because only make cluster runs initdb.
var inputs = []string{"postgres.wasm", "share.tar", "cluster.tar"}

func main() {
	in := flag.String("in", "build/out", "directory with postgres.wasm, share.tar and cluster.tar")
	out := flag.String("o", "../pgmem-engine/pg17", "engine directory to write the .zst files, SHA256SUMS, INPUTS.SHA256SUMS and PROVENANCE to")
	root := flag.String("root", ".", "the pgmem repository, with build/versions.env, the patches and the other build sources")
	flag.Parse()
	if err := run(*in, *out, *root); err != nil {
		fmt.Fprintln(os.Stderr, "mkassets:", err)
		os.Exit(1)
	}
}

func run(in, out, root string) error {
	// PROVENANCE comes first, so a bad versions file or source tree fails
	// before any engine file is replaced.
	prov, err := provenance(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	sums := map[string]string{}   // .zst file name -> its sha256
	inSums := map[string]string{} // input file name -> its sha256
	for _, name := range inputs {
		start := time.Now()
		data, err := os.ReadFile(filepath.Join(in, name))
		if err != nil {
			return err
		}
		hin := sha256.Sum256(data)
		inSums[name] = hex.EncodeToString(hin[:])
		enc, err := encode(data)
		if err != nil {
			return fmt.Errorf("encode %s: %w", name, err)
		}
		dst := filepath.Join(out, name+".zst")
		if err := writeAtomic(dst, enc); err != nil {
			return err
		}
		h := sha256.Sum256(enc)
		sums[name+".zst"] = hex.EncodeToString(h[:])
		fmt.Printf("wrote %s (%d -> %d bytes) in %v\n", dst, len(data), len(enc), time.Since(start).Round(time.Millisecond))
	}
	if err := writeAtomic(filepath.Join(out, "INPUTS.SHA256SUMS"), sumList(inSums)); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(out, "SHA256SUMS"), sumList(sums)); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(out, "PROVENANCE"), prov)
}

// provenanceGroups are the versions.env keys PROVENANCE lists, under a
// heading each. It leaves out the download checksums of the Go toolchain
// and the version of make.
var provenanceGroups = []struct {
	heading string
	keys    []string
}{
	{"PostgreSQL", []string{"PG_VERSION", "PG_SHA256"}},
	{"wasi-sdk", []string{"WASI_SDK_VERSION", "WASI_SDK_SHA256_ARM64", "WASI_SDK_SHA256_AMD64"}},
	{"Ubuntu snapshot", []string{"UBUNTU_DIGEST", "UBUNTU_SNAPSHOT"}},
	{"Build tools in the toolchain image", []string{"BISON_VERSION", "FLEX_VERSION", "GO_VERSION"}},
}

// sourceTree lists, relative to the repository root, the sources the
// build reads besides the downloads that versions.env pins. Each entry is
// a filepath.Match pattern, and a matching directory stands for every
// regular file under it. The compat layer's tests and their fixtures are
// left out, because the build doesn't read them.
var sourceTree = []string{
	"build/*.sh",
	"build/Containerfile",
	"build/objs.mk",
	"build/versions.env",
	"postgres/compat/include",
	"postgres/compat/src",
	"postgres/overlay",
	"postgres/patches",
}

// sourceTreeRule says in PROVENANCE how the source tree sha256 is made.
const sourceTreeRule = `  It hashes the regular files under these paths of the pgmem repository,
  except names that start with a dot, sorted by their path relative to the
  repository. For each file it hashes the length of the path in decimal
  and a newline, the path, the length of the contents in decimal and a
  newline, and the contents.
`

// provenance returns the PROVENANCE file for the repository at root: the
// pins in build/versions.env, the postgres/patches/*.patch files in the
// order the build applies them, and the sha256 of the source tree.
func provenance(root string) ([]byte, error) {
	versions := filepath.Join(root, "build", "versions.env")
	patches := filepath.Join(root, "postgres", "patches")
	env, err := readEnv(versions)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("Built by pgmem make assets, from the pgmem build inputs below.\n")
	for _, g := range provenanceGroups {
		fmt.Fprintf(&b, "\n%s\n", g.heading)
		for _, k := range g.keys {
			v, ok := env[k]
			if !ok {
				return nil, fmt.Errorf("%s: no %s", versions, k)
			}
			fmt.Fprintf(&b, "  %s=%s\n", k, v)
		}
	}
	files, err := filepath.Glob(filepath.Join(patches, "*.patch")) // sorted, as the build's shell glob
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no *.patch files", patches)
	}
	b.WriteString("\nPatches applied to PostgreSQL (sha256)\n")
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(data)
		fmt.Fprintf(&b, "  %s  %s\n", hex.EncodeToString(h[:]), filepath.Base(f))
	}
	sum, err := treeSum(root, sourceTree)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, "\nSource tree sha256\n  %s\n", sum)
	b.WriteString(sourceTreeRule)
	for _, pat := range sourceTree {
		fmt.Fprintf(&b, "    %s\n", pat)
	}
	return b.Bytes(), nil
}

// treeSum returns the sha256 of the regular files that patterns match
// under root, as sourceTreeRule describes. Paths are relative to root,
// with slashes. Names that start with a dot, such as .DS_Store, are
// skipped. A pattern that matches nothing, and a file that is neither a
// directory nor a regular file, are errors.
func treeSum(root string, patterns []string) (string, error) {
	seen := map[string]bool{}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("%s: no %s", root, pat)
		}
		for _, m := range matches {
			err := filepath.WalkDir(m, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if strings.HasPrefix(d.Name(), ".") {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if !d.Type().IsRegular() {
					return fmt.Errorf("%s is not a regular file", path)
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				seen[filepath.ToSlash(rel)] = true
				return nil
			})
			if err != nil {
				return "", err
			}
		}
	}
	h := sha256.New()
	for _, rel := range slices.Sorted(maps.Keys(seen)) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d\n%s%d\n", len(rel), rel, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readEnv reads a KEY=VALUE file such as build/versions.env. It skips
// blank lines and lines that start with #.
func readEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s:%d: %q is not KEY=VALUE", path, n, line)
		}
		env[k] = v
	}
	return env, sc.Err()
}

// sumList formats sums, which map file name to sha256, as sha256sum does,
// sorted by file name.
func sumList(sums map[string]string) []byte {
	var list strings.Builder
	for _, file := range slices.Sorted(maps.Keys(sums)) {
		fmt.Fprintf(&list, "%s  %s\n", sums[file], file)
	}
	return []byte(list.String())
}

// encode compresses data as one zstd frame with these fixed settings:
// level SpeedBetterCompression, a single goroutine, no checksum and no
// dictionary.
func encode(data []byte) ([]byte, error) {
	e, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false))
	if err != nil {
		return nil, err
	}
	defer e.Close()
	return e.EncodeAll(data, nil), nil
}

// writeAtomic writes data to path through a temporary file in the same
// directory, so path is either the old file or the complete new one.
func writeAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
