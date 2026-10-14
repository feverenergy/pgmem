package wasmrt_test

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestShareTarHasInitdbInputs(t *testing.T) {
	entries := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(readBuild(t, "share.tar")))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[h.Name] = h
	}
	for _, name := range []string{
		"share/postgresql/postgres.bki",
		"share/postgresql/system_constraints.sql",
		"share/postgresql/snowball_create.sql",
		"share/postgresql/tsearch_data/english.stop",
		"share/postgresql/extension/plpgsql.control",
		"share/postgresql/extension/btree_gist.control",
	} {
		if entries[name] == nil {
			t.Errorf("share.tar is missing %s", name)
		}
	}
	// dfmgr.c stat()s a library before loading it, so each statically
	// linked library needs a placeholder. build-postgres.sh lists them in
	// STATIC_LIBS.
	for _, lib := range staticLibs {
		if entries["lib/postgresql/"+lib+".so"] == nil {
			t.Errorf("share.tar is missing the placeholder lib/postgresql/%s.so", lib)
		}
	}
	if h := entries["share/timezone/UTC"]; h == nil || h.Typeflag != tar.TypeLink {
		t.Error("share/timezone/UTC is not a hard link")
	}
}

// staticLibs are the 30 libraries linked into postgres.wasm. They are
// three extensions, the regression suite's three and the 24 encoding
// conversion_procs.
var staticLibs = []string{
	"plpgsql", "dict_snowball", "btree_gist",
	"regress", "autoinc", "refint",
	"cyrillic_and_mic", "euc_cn_and_mic", "euc_jp_and_sjis",
	"euc_kr_and_mic", "euc_tw_and_big5", "latin2_and_win1250", "latin_and_mic",
	"utf8_and_big5", "utf8_and_cyrillic", "utf8_and_euc_cn",
	"utf8_and_euc_jp", "utf8_and_euc_kr", "utf8_and_euc_tw", "utf8_and_gb18030",
	"utf8_and_gbk", "utf8_and_iso8859", "utf8_and_iso8859_1", "utf8_and_johab",
	"utf8_and_sjis", "utf8_and_win", "utf8_and_uhc",
	"utf8_and_euc2004", "utf8_and_sjis2004", "euc2004_sjis2004",
}
