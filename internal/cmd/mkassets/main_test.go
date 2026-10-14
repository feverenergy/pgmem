package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// sample is 3 MiB of mixed input from a fixed seed. It holds zero pages,
// as in cluster.tar, text, and incompressible bytes.
func sample() []byte {
	r := rand.New(rand.NewPCG(1, 2))
	var b bytes.Buffer
	for b.Len() < 3<<20 {
		switch r.IntN(3) {
		case 0:
			b.Write(make([]byte, 8192))
		case 1:
			for range 200 {
				b.WriteString("CREATE TABLE t (a int, b text);\n")
			}
		case 2:
			p := make([]byte, 4096)
			for i := range p {
				p[i] = byte(r.Uint32())
			}
			b.Write(p)
		}
	}
	return b.Bytes()
}

// The same input encodes to the same bytes every time, and decodes back.
func TestEncodeDeterministic(t *testing.T) {
	in := sample()
	a, err := encode(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two encodings of the same input differ")
	}
	if len(a) >= len(in) {
		t.Fatalf("encoded %d bytes to %d", len(in), len(a))
	}
	d, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out, err := d.DecodeAll(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatal("decoding does not give the input back")
	}
}

// writeFiles writes each path -> content of files under dir, making the
// directories it needs.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// tree returns the files of a small repository: a versions.env, two
// patches and one file for each sourceTree entry. The files under
// postgres/compat/test and the .DS_Store are not in the source tree.
func tree() map[string]string {
	return map[string]string{
		"build/versions.env": `# pins
UBUNTU_DIGEST=sha256:aaaa
UBUNTU_SNAPSHOT=20260101T000000Z
BISON_VERSION=2:3.8.2
FLEX_VERSION=2.6.4
MAKE_VERSION=4.3

WASI_SDK_VERSION=34
WASI_SDK_SHA256_ARM64=arm
WASI_SDK_SHA256_AMD64=amd
GO_VERSION=1.27.0
PG_VERSION=17.11
PG_SHA256=pg
`,
		"build/build.sh":                "echo build\n",
		"build/Containerfile":           "FROM scratch\n",
		"build/objs.mk":                 "objs:\n",
		"build/README":                  "not in the tree\n",
		"postgres/compat/include/c.h":   "int c;\n",
		"postgres/compat/src/c.c":       "int c = 1;\n",
		"postgres/compat/test/t.c":      "not in the tree\n",
		"postgres/overlay/src/x/wasi.h": "#define WASI 1\n",
		"postgres/overlay/.DS_Store":    "not in the tree\n",
		"postgres/patches/0002-b.patch": "b\n",
		"postgres/patches/0001-a.patch": "a\n",
		"postgres/patches/README":       "not a patch\n",
	}
}

// wantProvenance is PROVENANCE for tree(). The source tree sha256 also
// comes from this shell command, run in a directory holding tree():
//
//	find build/*.sh build/Containerfile build/objs.mk build/versions.env \
//		postgres/compat/include postgres/compat/src postgres/overlay \
//		postgres/patches -type f ! -name '.*' | LC_ALL=C sort |
//	while read -r f; do
//		printf '%d\n%s%d\n' "${#f}" "$f" "$(wc -c <"$f")"; cat "$f"
//	done | shasum -a 256
const wantProvenance = `Built by pgmem make assets, from the pgmem build inputs below.

PostgreSQL
  PG_VERSION=17.11
  PG_SHA256=pg

wasi-sdk
  WASI_SDK_VERSION=34
  WASI_SDK_SHA256_ARM64=arm
  WASI_SDK_SHA256_AMD64=amd

Ubuntu snapshot
  UBUNTU_DIGEST=sha256:aaaa
  UBUNTU_SNAPSHOT=20260101T000000Z

Build tools in the toolchain image
  BISON_VERSION=2:3.8.2
  FLEX_VERSION=2.6.4
  GO_VERSION=1.27.0

Patches applied to PostgreSQL (sha256)
  87428fc522803d31065e7bce3cf03fe475096631e5e07bbd7a0fde60c4cf25c7  0001-a.patch
  0263829989b6fd954f72baaf2fc64bc2e2f01d692d4de72986ea808f6e99813f  0002-b.patch

Source tree sha256
  95e785caecc543ce2491685efc740ffb5e41dc1f24cf82363d8b84746694eb56
  It hashes the regular files under these paths of the pgmem repository,
  except names that start with a dot, sorted by their path relative to the
  repository. For each file it hashes the length of the path in decimal
  and a newline, the path, the length of the contents in decimal and a
  newline, and the contents.
    build/*.sh
    build/Containerfile
    build/objs.mk
    build/versions.env
    postgres/compat/include
    postgres/compat/src
    postgres/overlay
    postgres/patches
`

// buildRoot writes tree(), with the changes in edit applied, to a new
// directory and returns it. An empty value in edit removes that file.
func buildRoot(t *testing.T, edit map[string]string) string {
	t.Helper()
	files := tree()
	for name, data := range edit {
		if data == "" {
			delete(files, name)
		} else {
			files[name] = data
		}
	}
	root := t.TempDir()
	writeFiles(t, root, files)
	return root
}

// run writes one .zst per input, a sorted SHA256SUMS of them, a sorted
// INPUTS.SHA256SUMS of the inputs, and PROVENANCE.
func TestRunWritesSums(t *testing.T) {
	in, out := t.TempDir(), filepath.Join(t.TempDir(), "assets")
	root := buildRoot(t, nil)
	var wantIn bytes.Buffer
	for i, name := range []string{"cluster.tar", "postgres.wasm", "share.tar"} {
		data := bytes.Repeat([]byte{byte(i)}, 1000)
		if err := os.WriteFile(filepath.Join(in, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		wantIn.WriteString(hex.EncodeToString(h[:]) + "  " + name + "\n")
	}
	if err := run(in, out, root); err != nil {
		t.Fatal(err)
	}
	gotIn, err := os.ReadFile(filepath.Join(out, "INPUTS.SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotIn, wantIn.Bytes()) {
		t.Fatalf("INPUTS.SHA256SUMS:\n%s\nwant:\n%s", gotIn, wantIn.Bytes())
	}
	var want bytes.Buffer
	for _, name := range []string{"cluster.tar.zst", "postgres.wasm.zst", "share.tar.zst"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		want.WriteString(hex.EncodeToString(h[:]) + "  " + name + "\n")
	}
	got, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("SHA256SUMS:\n%s\nwant:\n%s", got, want.Bytes())
	}
	prov, err := os.ReadFile(filepath.Join(out, "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	if string(prov) != wantProvenance {
		t.Fatalf("PROVENANCE:\n%s\nwant:\n%s", prov, wantProvenance)
	}
	if err := run(t.TempDir(), out, root); err == nil {
		t.Fatal("run with no inputs succeeded")
	}
}

// Any file in the source tree changes its sha256, and files outside it
// don't.
func TestSourceTreeSum(t *testing.T) {
	base, err := treeSum(buildRoot(t, nil), sourceTree)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		file    string
		changes bool
	}{
		{"build/Containerfile", true},
		{"build/new.sh", true},
		{"postgres/compat/src/c.c", true},
		{"postgres/overlay/src/x/wasi.h", true},
		{"postgres/compat/test/t.c", false},
		{"postgres/overlay/.DS_Store", false},
		{"build/README", false},
	} {
		got, err := treeSum(buildRoot(t, map[string]string{tc.file: "changed\n"}), sourceTree)
		if err != nil {
			t.Fatal(err)
		}
		if (got != base) != tc.changes {
			t.Errorf("changing %s: sum changed %v, want %v", tc.file, got != base, tc.changes)
		}
	}
}

// A versions file without a listed key, a patch directory without
// patches, or a source path that is missing, fails before any file is
// written.
func TestProvenanceErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit map[string]string
		want string
	}{
		{"missing key", map[string]string{"build/versions.env": "PG_VERSION=17.11\n"}, "no PG_SHA256"},
		{"not KEY=VALUE", map[string]string{"build/versions.env": "PG_VERSION 17.11\n"}, "is not KEY=VALUE"},
		{"no patches", map[string]string{"postgres/patches/0001-a.patch": "", "postgres/patches/0002-b.patch": ""}, "no *.patch files"},
		{"no objs.mk", map[string]string{"build/objs.mk": ""}, "no build/objs.mk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "assets")
			err := run(t.TempDir(), out, buildRoot(t, tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run: %v, want an error with %q", err, tc.want)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("run wrote %s: %v", out, err)
			}
		})
	}
}
