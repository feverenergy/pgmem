package pgtest

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxLogLines is how many of a database's log lines a failing test prints.
const maxLogLines = 50

// logSlack is how much older than the test's start a log entry may look
// and still belong to the test. %m truncates to the millisecond, and the
// server's clock is the host's.
const logSlack = 2 * time.Millisecond

// pidField matches the [%p] field of the server's log_line_prefix,
// '%m [%p] %d '.
var pidField = regexp.MustCompile(`^\[[0-9]+\]$`)

// splitPrefix splits a log line into its prefix's timestamp (%m) and
// database (%d), and the message after the prefix. ok is false for a line
// with no prefix. The timestamp spans several space-separated fields
// ("2026-10-14 10:00:00.001 UTC"), so the database is the field after
// [%p], not one at a fixed position. It is empty for processes not
// connected to a database.
func splitPrefix(line string) (stamp, db, msg string, ok bool) {
	fields := strings.SplitN(line, " ", 6)
	for i := 1; i < len(fields)-2 && i < 4; i++ {
		if pidField.MatchString(fields[i]) {
			return strings.Join(fields[:i], " "), fields[i+1], strings.Join(fields[i+2:], " "), true
		}
	}
	return "", "", "", false
}

// zoneOffset matches a numeric time zone as %m writes one for a zone with
// no abbreviation: +02, -0130.
var zoneOffset = regexp.MustCompile(`^([+-])([0-9]{2})([0-9]{2})?$`)

// parseStamp parses a %m timestamp, "2026-10-14 10:00:00.001 UTC". The
// cluster logs in UTC, and parseStamp also reads GMT and numeric offsets.
// ok is false for anything else.
func parseStamp(stamp string) (t time.Time, ok bool) {
	f := strings.Fields(stamp)
	if len(f) != 3 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05.000", f[0]+" "+f[1], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	switch f[2] {
	case "UTC", "GMT":
		return t, true
	}
	m := zoneOffset.FindStringSubmatch(f[2])
	if m == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[2])
	mins := 0
	if m[3] != "" {
		mins, _ = strconv.Atoi(m[3])
	}
	off := time.Duration(h)*time.Hour + time.Duration(mins)*time.Minute
	if m[1] == "-" {
		off = -off
	}
	return t.Add(-off), true
}

// isContinuation reports whether line continues the entry before it. That
// is a line of a multi-line message, which starts with a tab, or the
// DETAIL, HINT, STATEMENT or CONTEXT of an error, which Postgres writes
// after the error with a prefix of its own.
func isContinuation(line string) bool {
	if strings.HasPrefix(line, "\t") {
		return true
	}
	_, _, msg, ok := splitPrefix(line)
	if !ok {
		return false
	}
	for _, kw := range []string{"DETAIL:", "HINT:", "STATEMENT:", "CONTEXT:"} {
		if strings.HasPrefix(msg, kw) {
			return true
		}
	}
	return false
}

// serverProcessDied matches the postmaster's message for a backend that
// died: "server process (PID 9) was terminated by signal 11".
var serverProcessDied = regexp.MustCompile(`^[A-Z]+:  server process \(PID [0-9]+\) .*terminated by`)

// isCrash reports whether line says a server process died. That ends
// every session on the server, whatever its database. A line counts if its
// message is the postmaster's "server process (PID n) ... terminated by",
// or if it is one of the postmaster's lines, which name no database.
// Another database's error that quotes such words is not a crash.
func isCrash(line string) bool {
	_, db, msg, ok := splitPrefix(line)
	if !ok {
		return false
	}
	if serverProcessDied.MatchString(msg) {
		return true
	}
	return db == "" && (strings.Contains(msg, "terminated by signal") || strings.Contains(msg, "was terminated"))
}

// logLines returns the lines of the server log log that a failing test on
// database db should see, in log order. These are the last max lines of
// db's entries logged since since, and every entry that reports a crash,
// whatever its database and time. An entry is a line whose prefix names
// db, with its continuation lines. A recycled database serves many tests,
// and since, the test's start, leaves out the earlier tests' entries.
// logLines keeps an entry whose timestamp cannot be read. When the cap
// falls inside an entry, it leaves out the rest of that entry too.
func logLines(log, db string, since time.Time, max int) []string {
	type line struct {
		text  string
		crash bool // part of a crash entry, always kept
		cont  bool // a continuation line
	}
	var kept []line
	var nDB int
	keep, crash := false, false // whether the current entry is kept, and why
	for _, text := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		cont := isContinuation(text)
		if !cont {
			stamp, name, _, _ := splitPrefix(text)
			crash = isCrash(text)
			keep = crash || name == db && isSince(stamp, since)
		}
		if keep {
			kept = append(kept, line{text, crash, cont})
			if !crash {
				nDB++
			}
		}
	}
	var out []string
	skip := nDB - max // the oldest database lines beyond max
	for i, l := range kept {
		if !l.crash && skip > 0 {
			skip--
			if skip == 0 {
				// Also skip the rest of the entry the cap fell in.
				for j := i + 1; j < len(kept) && kept[j].cont && !kept[j].crash; j++ {
					skip++
				}
			}
			continue
		}
		out = append(out, l.text)
	}
	return out
}

// isSince reports whether an entry with timestamp stamp was logged at or
// after since, give or take logSlack. A zero since, or a timestamp that
// cannot be read, counts as since.
func isSince(stamp string, since time.Time) bool {
	if since.IsZero() {
		return true
	}
	t, ok := parseStamp(stamp)
	return !ok || !t.Before(since.Add(-logSlack))
}
