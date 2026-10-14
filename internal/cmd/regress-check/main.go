// Command regress-check compares a pg_regress run with the allowlist of
// known differences.
//
//	regress-check [-allowlist postgres/regress-allowlist] [-exit N] OUTDIR
//
// It reads regression.out and regression.diffs from pg_regress's output
// directory OUTDIR. regression.out says which tests ran and which failed.
// regress-check splits the diffs per test. Each test's section starts with
// the "diff -U3 EXPECTED RESULTS" line pg_regress writes. The test takes
// its name from its RESULTS file, because EXPECTED may be an alternative
// such as json_1.out. Normalizing a section drops that line and the
// "---"/"+++" lines after it, which carry paths and timestamps. The sha256
// of the rest identifies the difference.
//
// The allowlist has one line per test, "<test> <sha256> <reason>", plus
// blank lines and # comments. The run fails on any of these:
//
//   - a failing test that is not listed;
//   - a listed test whose diff hash changed;
//   - a listed test that passes or did not run.
//
// pg_regress removes both files when every test passes, so their absence
// means that every test passed. The exception is when -exit says
// pg_regress exited with status 1, which means some test failed. Missing
// files are then an error, not a pass.
//
// The exit status is 0 if the run matches the allowlist, 1 if it does not
// and 2 on a usage or read error, or output that pg_regress did not finish.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func main() {
	allowlist := flag.String("allowlist", "postgres/regress-allowlist", "the allowlist `FILE`")
	exit := flag.Int("exit", -1, "pg_regress's exit status `N`, if known")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: regress-check [-allowlist FILE] [-exit N] OUTDIR")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	ok, err := run(flag.Arg(0), *allowlist, *exit, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "regress-check:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

type entry struct {
	hash, reason string
}

// run checks the pg_regress output in dir against the allowlist file and
// writes a report to w. It returns whether the run matches. pgRegressExit
// is pg_regress's exit status, or -1 if unknown.
func run(dir, allowlistFile string, pgRegressExit int, w io.Writer) (bool, error) {
	b, err := os.ReadFile(allowlistFile)
	if err != nil {
		return false, err
	}
	allow, err := parseAllowlist(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", allowlistFile, err)
	}
	diffsFile := filepath.Join(dir, "regression.diffs")
	outFile := filepath.Join(dir, "regression.out")
	diffsB, diffsErr := os.ReadFile(diffsFile)
	outB, outErr := os.ReadFile(outFile)
	for _, e := range []error{diffsErr, outErr} {
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return false, e
		}
	}

	// results is nil when pg_regress removed its files, because every test
	// passed.
	var results map[string]bool
	diffs := map[string]string{}
	switch {
	case outErr == nil:
		if results, err = parseResults(outB); err != nil {
			return false, fmt.Errorf("%s: %w", outFile, err)
		}
		if diffsErr == nil {
			if diffs, err = splitDiffs(diffsB); err != nil {
				return false, fmt.Errorf("%s: %w", diffsFile, err)
			}
		}
	case diffsErr == nil:
		return false, fmt.Errorf("%s exists but %s does not", diffsFile, outFile)
	case pgRegressExit == 1:
		// Not every test passed, yet the results are gone. That is never a
		// pass.
		return false, fmt.Errorf("pg_regress exited with status 1 (a test failed) but left no %s", outFile)
	}
	for name := range diffs {
		if ok, ran := results[name]; !ran || ok {
			return false, fmt.Errorf("%s has a diff for %s, which regression.out does not list as failed", diffsFile, name)
		}
	}

	var problems []string
	failed, allowed := 0, 0
	for _, name := range sortedKeys(results) {
		if results[name] {
			continue
		}
		failed++
		h, ok := diffs[name]
		if !ok {
			h = hashOf(nil) // the test process failed, but its output matched
		}
		e, listed := allow[name]
		switch {
		case !listed:
			problems = append(problems, fmt.Sprintf("%s failed and is not in the allowlist; its diff hash is %s", name, h))
		case e.hash != h:
			problems = append(problems, fmt.Sprintf("%s: diff changed: hash %s, allowlisted %s", name, h, e.hash))
		default:
			allowed++
			fmt.Fprintf(w, "regress-check: %s failed as allowlisted: %s\n", name, e.reason)
		}
	}
	for _, name := range sortedKeys(allow) {
		ok, ran := results[name]
		switch {
		case results == nil || ran && ok:
			problems = append(problems, fmt.Sprintf("%s is in the allowlist but now passes: remove it", name))
		case !ran:
			problems = append(problems, fmt.Sprintf("%s is in the allowlist but did not run", name))
		}
	}

	for _, p := range problems {
		fmt.Fprintf(w, "regress-check: FAIL %s\n", p)
	}
	verdict := "OK"
	if len(problems) > 0 {
		verdict = fmt.Sprintf("%d problems: FAIL (diffs in %s)", len(problems), diffsFile)
	}
	if results == nil {
		fmt.Fprintf(w, "regress-check: every test passed; %s\n", verdict)
	} else {
		fmt.Fprintf(w, "regress-check: %d of %d tests passed; %d failed, %d allowlisted; %s\n",
			len(results)-failed, len(results), failed, allowed, verdict)
	}
	return len(problems) == 0, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// splitDiffs splits regression.diffs into per-test sections and returns
// the sha256 of each test's normalized sections.
func splitDiffs(b []byte) (map[string]string, error) {
	bodies := map[string]*bytes.Buffer{}
	var cur *bytes.Buffer
	skip := 0 // header lines still to drop after a "diff" line
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 64<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.HasPrefix(line, "diff ") {
			name, err := sectionTest(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
			if bodies[name] == nil {
				bodies[name] = new(bytes.Buffer)
			}
			cur, skip = bodies[name], 2
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: a diff line before any \"diff\" header", n)
		}
		if skip == 2 && strings.HasPrefix(line, "--- ") || skip == 1 && strings.HasPrefix(line, "+++ ") {
			skip--
			continue
		}
		skip = 0
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	hashes := make(map[string]string, len(bodies))
	for name, body := range bodies {
		hashes[name] = hashOf(body.Bytes())
	}
	return hashes, nil
}

// sectionTest names the test of a "diff -U3 EXPECTED RESULTS" header line
// after its results file, OUTDIR/results/<test>.out.
func sectionTest(line string) (string, error) {
	i := strings.LastIndex(line, "/results/")
	if i < 0 {
		return "", fmt.Errorf("diff header %q names no results/ file", line)
	}
	name, ok := strings.CutSuffix(line[i+len("/results/"):], ".out")
	if !ok || name == "" || strings.ContainsAny(name, "/ ") {
		return "", fmt.Errorf("diff header %q: cannot name the test", line)
	}
	return name, nil
}

var (
	statusRE = regexp.MustCompile(`^(not )?ok\s+\d+\s+[-+]\s+(\S+)\s+\d+ ms$`)
	planRE   = regexp.MustCompile(`^1\.\.(\d+)$`)
)

// parseResults reads pg_regress's TAP summary, which says whether each
// test passed. It fails unless pg_regress finished. Finishing means no
// bail-out, and a plan line that counts every test.
func parseResults(b []byte) (map[string]bool, error) {
	results := map[string]bool{}
	plan := -1
	for n, line := range strings.Split(string(b), "\n") {
		// pg_regress writes "# <reason>Bail out!" to its log.
		if strings.Contains(line, "Bail out!") {
			return nil, fmt.Errorf("line %d: pg_regress bailed out: %s", n+1, line)
		}
		if m := planRE.FindStringSubmatch(line); m != nil {
			plan, _ = strconv.Atoi(m[1])
			continue
		}
		m := statusRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, dup := results[m[2]]; dup {
			return nil, fmt.Errorf("line %d: test %s listed twice", n+1, m[2])
		}
		results[m[2]] = m[1] == ""
	}
	switch {
	case plan < 0:
		return nil, errors.New("no TAP plan line: pg_regress did not finish")
	case plan != len(results):
		return nil, fmt.Errorf("the plan counts %d tests but %d are listed", plan, len(results))
	}
	return results, nil
}

// parseAllowlist reads "<test> <sha256> <reason>" lines.
func parseAllowlist(b []byte) (map[string]entry, error) {
	allow := map[string]entry{}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("line %d: want \"<test> <sha256> <reason>\", got %q", n+1, line)
		}
		if h, err := hex.DecodeString(f[1]); err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("line %d: %q is not a sha256 in hex", n+1, f[1])
		}
		if _, dup := allow[f[0]]; dup {
			return nil, fmt.Errorf("line %d: %s is listed twice", n+1, f[0])
		}
		allow[f[0]] = entry{hash: strings.ToLower(f[1]), reason: strings.Join(f[2:], " ")}
	}
	return allow, nil
}
