package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake run has two failing tests. One of them has two diff sections'
// worth of hunks. Paths and timestamps differ from run to run.
const diffsA = `diff -U3 /src/A/expected/select_parallel.out /out/A/results/select_parallel.out
--- /src/A/expected/select_parallel.out	2026-08-10 22:54:09
+++ /out/A/results/select_parallel.out	2026-10-14 00:30:45
@@ -1,3 +1,3 @@
 explain
--- a SQL comment line that diff marks as removed
-  Workers Launched: 4
+  Workers Launched: 0
diff -U3 /src/A/expected/json_1.out /out/A/results/json.out
--- /src/A/expected/json_1.out	2026-08-10 22:54:09
+++ /out/A/results/json.out	2026-10-14 00:30:46
@@ -10,2 +10,2 @@
-ERROR:  stack depth limit exceeded
+ERROR:  invalid input syntax for type json
`

// The same failures from another run, with other directories and other
// times.
const diffsB = `diff -U3 /elsewhere/expected/select_parallel.out /tmp/run2/results/select_parallel.out
--- /elsewhere/expected/select_parallel.out	2026-08-10 22:54:09
+++ /tmp/run2/results/select_parallel.out	2026-10-15 09:01:02
@@ -1,3 +1,3 @@
 explain
--- a SQL comment line that diff marks as removed
-  Workers Launched: 4
+  Workers Launched: 0
diff -U3 /elsewhere/expected/json_1.out /tmp/run2/results/json.out
--- /elsewhere/expected/json_1.out	2026-08-10 22:54:09
+++ /tmp/run2/results/json.out	2026-10-15 09:01:03
@@ -10,2 +10,2 @@
-ERROR:  stack depth limit exceeded
+ERROR:  invalid input syntax for type json
`

const outFailing = `# using postmaster on /tmp/pgmr.abcd, port 54329
ok 1         - test_setup                                128 ms
# parallel group (3 tests):  boolean json char
ok 2         + boolean                                    43 ms
not ok 3     + json                                       79 ms
ok 4         + char                                       26 ms
not ok 5     - select_parallel                           237 ms
1..5
# 2 of 5 tests failed.
`

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var (
	spHash   = sum("@@ -1,3 +1,3 @@\n explain\n--- a SQL comment line that diff marks as removed\n-  Workers Launched: 4\n+  Workers Launched: 0\n")
	jsonHash = sum("@@ -10,2 +10,2 @@\n-ERROR:  stack depth limit exceeded\n+ERROR:  invalid input syntax for type json\n")
)

func writeRun(t *testing.T, diffs, out string) string {
	t.Helper()
	dir := t.TempDir()
	if diffs != "" {
		if err := os.WriteFile(filepath.Join(dir, "regression.diffs"), []byte(diffs), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out != "" {
		if err := os.WriteFile(filepath.Join(dir, "regression.out"), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeAllowlist(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "regress-allowlist")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSplitDiffs(t *testing.T) {
	got, err := splitDiffs([]byte(diffsA))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sections, want 2: %v", len(got), got)
	}
	if got["select_parallel"] != spHash {
		t.Errorf("select_parallel hash %s, want %s", got["select_parallel"], spHash)
	}
	// The expected file was an alternative, json_1.out. The test takes its
	// name from its results file.
	if got["json"] != jsonHash {
		t.Errorf("json hash %s, want %s", got["json"], jsonHash)
	}
}

func TestSplitDiffsIgnoresPathsAndTimes(t *testing.T) {
	a, err := splitDiffs([]byte(diffsA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := splitDiffs([]byte(diffsB))
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range a {
		if b[name] != h {
			t.Errorf("%s: hash %s in run A, %s in run B", name, h, b[name])
		}
	}
}

func TestSplitDiffsBodyChangeChangesHash(t *testing.T) {
	changed := strings.Replace(diffsA, "+  Workers Launched: 0", "+  Workers Launched: 1", 1)
	got, err := splitDiffs([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	if got["select_parallel"] == spHash {
		t.Error("a changed diff body kept its hash")
	}
	if got["json"] != jsonHash {
		t.Error("an unchanged section changed its hash")
	}
}

func TestSplitDiffsRejectsGarbage(t *testing.T) {
	for _, s := range []string{
		"@@ -1 +1 @@\n-a\n+b\n", // a body before any header
		"diff -U3 /x/expected/a.out /x/elsewhere/a.out\n--- x\n+++ y\n", // no results/ directory
	} {
		if _, err := splitDiffs([]byte(s)); err == nil {
			t.Errorf("splitDiffs(%q) succeeded", s)
		}
	}
}

func TestParseResults(t *testing.T) {
	r, err := parseResults([]byte(outFailing))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"test_setup": true, "boolean": true, "json": false, "char": true, "select_parallel": false}
	if len(r) != len(want) {
		t.Fatalf("got %v, want %v", r, want)
	}
	for name, ok := range want {
		if got, found := r[name]; !found || got != ok {
			t.Errorf("%s: got ok=%v (found %v), want ok=%v", name, got, found, ok)
		}
	}
}

func TestParseResultsIncomplete(t *testing.T) {
	for name, s := range map[string]string{
		"no plan": strings.Replace(outFailing, "1..5\n", "", 1),
		// This is pg_regress's own format. The plan line stays, so only the
		// bail-out can fail it.
		"bail out": strings.Replace(outFailing, "1..5\n", "1..5\n# could not stop postmaster: exit code was 256Bail out!\n", 1),
		"short":    strings.Replace(outFailing, "1..5\n", "1..6\n", 1),
	} {
		_, err := parseResults([]byte(s))
		if err == nil {
			t.Errorf("%s: parseResults succeeded", name)
		} else if name == "bail out" && !strings.Contains(err.Error(), "bailed out") {
			t.Errorf("%s: %v, want it named as a bail-out", name, err)
		}
	}
}

func TestParseAllowlist(t *testing.T) {
	al, err := parseAllowlist([]byte("# comment\n\nselect_parallel " + spHash + " parallel query is off\njson " + jsonHash + " known bug: stack depth\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(al) != 2 || al["select_parallel"].hash != spHash || al["json"].reason != "known bug: stack depth" {
		t.Fatalf("got %+v", al)
	}
	for _, bad := range []string{
		"json " + jsonHash + "\n",                            // no reason
		"json abc reason\n",                                  // not a sha256
		"json " + jsonHash + " a\njson " + jsonHash + " b\n", // listed twice
	} {
		if _, err := parseAllowlist([]byte(bad)); err == nil {
			t.Errorf("parseAllowlist(%q) succeeded", bad)
		}
	}
}

func check(t *testing.T, dir, allowlist string) (bool, string) {
	t.Helper()
	var buf bytes.Buffer
	ok, err := run(dir, allowlist, -1, &buf)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	return ok, buf.String()
}

func TestRunAllAllowlisted(t *testing.T) {
	dir := writeRun(t, diffsA, outFailing)
	al := writeAllowlist(t, "select_parallel "+spHash+" parallel query is off\njson "+jsonHash+" known bug\n")
	ok, out := check(t, dir, al)
	if !ok {
		t.Fatalf("failed:\n%s", out)
	}
	if !strings.Contains(out, "3 of 5 tests passed") || !strings.Contains(out, "2 allowlisted") {
		t.Errorf("report lacks the summary:\n%s", out)
	}
}

// Rule 1. A failing test that is not listed fails the run.
func TestRunUnlistedFailure(t *testing.T) {
	dir := writeRun(t, diffsA, outFailing)
	al := writeAllowlist(t, "select_parallel "+spHash+" parallel query is off\n")
	ok, out := check(t, dir, al)
	if ok {
		t.Fatalf("passed:\n%s", out)
	}
	if !strings.Contains(out, "json") || !strings.Contains(out, jsonHash) || !strings.Contains(out, "not in the allowlist") {
		t.Errorf("report does not name json, its hash and the rule:\n%s", out)
	}
}

// Rule 2. A listed test whose diff changed fails the run.
func TestRunChangedDiff(t *testing.T) {
	dir := writeRun(t, strings.Replace(diffsA, "type json", "type jsonb", 1), outFailing)
	al := writeAllowlist(t, "select_parallel "+spHash+" parallel query is off\njson "+jsonHash+" known bug\n")
	ok, out := check(t, dir, al)
	if ok {
		t.Fatalf("passed:\n%s", out)
	}
	if !strings.Contains(out, "json") || !strings.Contains(out, "diff changed") {
		t.Errorf("report does not name json's changed diff:\n%s", out)
	}
}

// Rule 3. A listed test that passes fails the run, and so does one that
// did not run at all.
func TestRunListedTestPasses(t *testing.T) {
	dir := writeRun(t, diffsA, outFailing)
	al := writeAllowlist(t, "select_parallel "+spHash+" parallel query is off\njson "+jsonHash+" known bug\n"+
		"char "+sum("x")+" was broken\nsubscription "+sum("y")+" not linked\n")
	ok, out := check(t, dir, al)
	if ok {
		t.Fatalf("passed:\n%s", out)
	}
	if !strings.Contains(out, "char") || !strings.Contains(out, "now passes") {
		t.Errorf("report does not name char as passing:\n%s", out)
	}
	if !strings.Contains(out, "subscription") || !strings.Contains(out, "did not run") {
		t.Errorf("report does not name subscription as not run:\n%s", out)
	}
}

// pg_regress removes both files when every test passes. Every listed test
// then passes too.
func TestRunNoFiles(t *testing.T) {
	dir := writeRun(t, "", "")
	ok, out := check(t, dir, writeAllowlist(t, "# nothing\n"))
	if !ok {
		t.Fatalf("an empty allowlist and no failures failed:\n%s", out)
	}
	ok, out = check(t, dir, writeAllowlist(t, "json "+jsonHash+" known bug\n"))
	if ok || !strings.Contains(out, "now passes") {
		t.Fatalf("a listed test with no failures did not fail the run:\n%s", out)
	}
}

func TestRunInconsistent(t *testing.T) {
	al := writeAllowlist(t, "select_parallel "+spHash+" r\njson "+jsonHash+" r\n")
	// json has a diff but regression.out says it passed.
	dir := writeRun(t, diffsA, strings.Replace(outFailing, "not ok 3     + json", "ok 3         + json", 1))
	var buf bytes.Buffer
	if _, err := run(dir, al, -1, &buf); err == nil {
		t.Errorf("a diff for a passing test was accepted:\n%s", buf.String())
	}
	// A diff file without regression.out.
	dir = writeRun(t, diffsA, "")
	if _, err := run(dir, al, -1, &buf); err == nil {
		t.Error("regression.diffs without regression.out was accepted")
	}
}

// A failing test with no diff section hashes as an empty diff. Its process
// failed, but its output matched.
func TestRunFailureWithoutDiff(t *testing.T) {
	dir := writeRun(t, strings.SplitAfter(diffsA, "+  Workers Launched: 0\n")[0], outFailing)
	al := writeAllowlist(t, "select_parallel "+spHash+" r\njson "+jsonHash+" r\n")
	ok, out := check(t, dir, al)
	if ok || !strings.Contains(out, sum("")) {
		t.Fatalf("want a failure with the empty hash:\n%s", out)
	}
}

// pg_regress exiting with status 1 means a test failed. Missing result
// files are then an error, never "every test passed".
func TestRunFailedExitWithoutFiles(t *testing.T) {
	dir := writeRun(t, "", "")
	al := writeAllowlist(t, "# nothing\n")
	var buf bytes.Buffer
	if _, err := run(dir, al, 1, &buf); err == nil {
		t.Fatalf("pg_regress exit 1 without regression.out was accepted:\n%s", buf.String())
	}
	for _, exit := range []int{0, -1} {
		if ok, err := run(dir, al, exit, &buf); err != nil || !ok {
			t.Errorf("exit %d without files: ok %v, err %v; want a pass", exit, ok, err)
		}
	}
}
