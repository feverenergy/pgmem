package memfs_test

import (
	"archive/tar"
	"bytes"
	"testing"

	"github.com/feverenergy/pgmem/internal/memfs"
)

func TestLoadTarKeepsLinks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	add(&tar.Header{Name: "share/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "share/timezone/Etc/UTC", Typeflag: tar.TypeReg, Mode: 0o644}, "TZif")
	add(&tar.Header{Name: "share/timezone/UTC", Typeflag: tar.TypeLink, Linkname: "share/timezone/Etc/UTC"}, "")
	add(&tar.Header{Name: "share/latest", Typeflag: tar.TypeSymlink, Linkname: "timezone"}, "")
	tw.Close()

	fs := memfs.New()
	if err := fs.LoadTar(&buf, "/pgmem"); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("/pgmem/share/timezone/UTC")
	if err != nil || string(got) != "TZif" {
		t.Fatalf("hard link: %q %v", got, err)
	}
	a, _ := fs.Lookup(fs.Root(), "/pgmem/share/timezone/UTC", true)
	b, _ := fs.Lookup(fs.Root(), "/pgmem/share/timezone/Etc/UTC", true)
	if a != b || fs.Stat(a).Nlink != 2 {
		t.Fatal("hard link loaded as a copy")
	}
	if got, err := fs.ReadFile("/pgmem/share/latest/Etc/UTC"); err != nil || string(got) != "TZif" {
		t.Fatalf("symlink: %q %v", got, err)
	}
}

func TestWriteTarRoundTrip(t *testing.T) {
	src := memfs.New()
	for path, body := range map[string]string{
		"/c/global/pg_control": "control",
		"/c/base/1/112":        "page",
		"/c/PG_VERSION":        "17\n",
	} {
		if err := src.WriteFile(path, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"/c/pg_notify", "/c/pg_tblspc"} {
		if err := src.Mkdir(src.Root(), dir); err != 0 {
			t.Fatal(err)
		}
	}
	if err := src.Link(src.Root(), "/c/PG_VERSION", src.Root(), "/c/base/PG_VERSION", false); err != 0 {
		t.Fatal(err)
	}
	if err := src.Symlink("base/1", src.Root(), "/c/latest"); err != 0 {
		t.Fatal(err)
	}

	var a, b bytes.Buffer
	if err := src.WriteTar(&a, "/c"); err != nil {
		t.Fatal(err)
	}
	if err := src.WriteTar(&b, "/c"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("WriteTar is not deterministic")
	}

	dst := memfs.New()
	if err := dst.LoadTar(bytes.NewReader(a.Bytes()), "/d"); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"/d/global/pg_control": "control",
		"/d/base/1/112":        "page",
		"/d/latest/112":        "page",
		"/d/base/PG_VERSION":   "17\n",
	} {
		got, err := dst.ReadFile(path)
		if err != nil || string(got) != body {
			t.Errorf("%s: %q %v", path, got, err)
		}
	}
	for _, dir := range []string{"/d/pg_notify", "/d/pg_tblspc"} {
		n, err := dst.Lookup(dst.Root(), dir, true)
		if err != 0 || dst.Stat(n).Kind != memfs.KindDir {
			t.Errorf("empty directory %s lost", dir)
		}
	}
	v1, _ := dst.Lookup(dst.Root(), "/d/PG_VERSION", true)
	v2, _ := dst.Lookup(dst.Root(), "/d/base/PG_VERSION", true)
	if v1 != v2 {
		t.Error("hard link came back as a copy")
	}
	if l, _ := dst.Lookup(dst.Root(), "/d/latest", false); dst.Stat(l).Kind != memfs.KindSymlink {
		t.Error("symlink came back as something else")
	}
	if err := src.WriteTar(&a, "/c/PG_VERSION"); err == nil {
		t.Error("WriteTar of a file should fail")
	}
}
