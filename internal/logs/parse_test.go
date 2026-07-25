package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestParseJobFailure(t *testing.T) {
	raw := readFixture(t, "job_failure.log")
	secs := Parse(raw)

	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(secs), secs)
	}

	if secs[0].Header != "Run actions/checkout@v4" {
		t.Errorf("section 0 header = %q", secs[0].Header)
	}
	if secs[0].HasError {
		t.Errorf("section 0 should not have an error")
	}
	if secs[0].Kind() != model.KindAction {
		t.Errorf("section 0 kind = %q, want action", secs[0].Kind())
	}

	test := secs[1]
	if test.Header != "Run go test ./..." {
		t.Errorf("section 1 header = %q", test.Header)
	}
	if !test.HasError {
		t.Errorf("section 1 should have an error")
	}
	if test.Kind() != model.KindBash {
		t.Errorf("section 1 kind = %q, want bash", test.Kind())
	}
	if test.Command() != "go test ./..." {
		t.Errorf("section 1 command = %q", test.Command())
	}
	body := strings.Join(test.Body, "\n")
	if strings.Contains(body, "2024-05-01") {
		t.Errorf("timestamps not stripped from body: %q", body)
	}
	if !strings.Contains(body, "--- FAIL: TestThing") {
		t.Errorf("body missing failure line: %q", body)
	}
	if !strings.Contains(body, "##[error]Process completed") {
		t.Errorf("body missing error marker: %q", body)
	}
}

func TestErrorSections(t *testing.T) {
	secs := Parse(readFixture(t, "job_failure.log"))
	errs := ErrorSections(secs)
	if len(errs) != 1 {
		t.Fatalf("got %d error sections, want 1", len(errs))
	}
	if errs[0].Header != "Run go test ./..." {
		t.Errorf("error section header = %q", errs[0].Header)
	}
}

func TestParseLeadingSection(t *testing.T) {
	raw := "2024-05-01T10:00:00.0000000Z preamble line\n2024-05-01T10:00:01.0000000Z ##[group]Run echo hi\n2024-05-01T10:00:01.5000000Z ##[endgroup]\n2024-05-01T10:00:02.0000000Z hi\n"
	secs := Parse(raw)
	if len(secs) != 2 {
		t.Fatalf("got %d sections, want 2", len(secs))
	}
	if secs[0].Header != "" || len(secs[0].Body) == 0 || secs[0].Body[0] != "preamble line" {
		t.Errorf("leading section = %+v", secs[0])
	}
}

// githubStampLen reports how many bytes of the "<ISO-8601>Z " prefix GitHub
// puts at the head of a log line, or 0 when the line does not open with one.
//
// It is spelled out by hand — digit by digit, separator by separator — for one
// reason: it must not be the production regexp. An oracle that asked tsPrefix
// what a timestamp is would agree with StripTimestamps under any redefinition
// of tsPrefix, so widening the shape to "\d+-\d+-\d+…", loosening the separator
// to "Z\s", or letting the match float past leading indentation would all read
// as correct. This is the shape GitHub actually emits, written down once:
// 4-2-2 date, "T", 2:2:2 time, optional "." plus at least one fraction digit,
// "Z", one space — ASCII digits only, flush against the start of the line.
func githubStampLen(line string) int {
	rest := line
	digits := func(n int) bool {
		if len(rest) < n {
			return false
		}
		for i := range n {
			if rest[i] < '0' || rest[i] > '9' {
				return false
			}
		}
		rest = rest[n:]
		return true
	}
	lit := func(c byte) bool {
		if rest == "" || rest[0] != c {
			return false
		}
		rest = rest[1:]
		return true
	}
	shaped := digits(4) && lit('-') && digits(2) && lit('-') && digits(2) && lit('T') &&
		digits(2) && lit(':') && digits(2) && lit(':') && digits(2)
	if !shaped {
		return 0
	}
	if lit('.') { // the fraction is optional, but "." alone is not a timestamp
		n := 0
		for rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			rest, n = rest[1:], n+1
		}
		if n == 0 {
			return 0
		}
	}
	if !lit('Z') || !lit(' ') {
		return 0
	}
	return len(line) - len(rest)
}

// TestStripTimestamps pins the prefix contract line by line. The monitor puts a
// short raw log straight into a CI-failure event, so whatever this returns is
// what an agent reads: a chewed-up tail or a lost line is a wrong answer, not a
// cosmetic one. Only a full "<ISO-8601>Z " at the very start of a line is
// GitHub's; everything else on the line is somebody's output.
func TestStripTimestamps(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty input", "", ""},
		{"fractional seconds", "2024-05-01T10:00:03.5000000Z --- FAIL: TestThing", "--- FAIL: TestThing"},
		{"fraction is optional", "2024-05-01T10:00:03Z --- FAIL: TestThing", "--- FAIL: TestThing"},
		{"single fractional digit", "2024-05-01T10:00:03.5Z boom", "boom"},
		{"untimestamped line comes back byte-identical", "  --- FAIL: TestThing\t(0.00s)  ", "  --- FAIL: TestThing\t(0.00s)  "},
		{"only the separator space goes, indentation stays", "2024-05-01T10:00:03.6000000Z     thing_test.go:42: expected 1 got 2", "    thing_test.go:42: expected 1 got 2"},
		{"a timestamp mid-line is content, not a prefix", "deploy started at 2024-05-01T10:00:03.5000000Z and failed", "deploy started at 2024-05-01T10:00:03.5000000Z and failed"},
		{"no separator space is not a prefix", "2024-05-01T10:00:03.5000000Zboom", "2024-05-01T10:00:03.5000000Zboom"},
		// GitHub separates the stamp from the payload with exactly one space. A
		// looser separator (`Z\s`) would swallow a tab that belongs to the
		// payload — and indented, tab-aligned output is how test failures read.
		{"a tab is payload, not the separator", "2024-05-01T10:00:03.5000000Z\tgo test output", "2024-05-01T10:00:03.5000000Z\tgo test output"},
		// Indentation is load-bearing (see the stack-trace case above), so a
		// stamp must sit flush at column 0 to be GitHub's. Anything indented is
		// a tool's own stamped output, and eating the indentation with it would
		// reflow somebody's log.
		{"an indented timestamp is not GitHub's", "  2024-05-01T10:00:03.5000000Z indented", "  2024-05-01T10:00:03.5000000Z indented"},
		{"a tab-indented timestamp is not GitHub's", "\t2024-05-01T10:00:03.5000000Z indented", "\t2024-05-01T10:00:03.5000000Z indented"},
		// The field widths are fixed. Without them a bare "1-2-3T4:5:6Z " —
		// which no ISO-8601 emitter produces but plenty of prose does — would be
		// cut off the front of a line.
		{"loose field widths are not a timestamp", "1-2-3T4:5:6Z boom", "1-2-3T4:5:6Z boom"},
		{"a five-digit year is not a timestamp", "20245-05-01T10:00:03Z boom", "20245-05-01T10:00:03Z boom"},
		{"a lone fraction point is not a timestamp", "2024-05-01T10:00:03.Z boom", "2024-05-01T10:00:03.Z boom"},
		// Digits means ASCII digits: a payload that opens with non-ASCII
		// lookalikes is content.
		{"non-ASCII digits are not a timestamp", "٢٠٢٤-٠٥-٠١T١٠:٠٠:٠٣Z boom", "٢٠٢٤-٠٥-٠١T١٠:٠٠:٠٣Z boom"},
		// The result feeds distil.CapSummary, whose cut is UTF-8-safe only if
		// what it is handed is still valid UTF-8: stripping must count bytes off
		// the front, never runes off a multibyte payload.
		{"multibyte payload survives byte for byte", "2024-05-01T10:00:03.5000000Z ✗ échec — 你好 🙂", "✗ échec — 你好 🙂"},
		{"bare timestamp with nothing after it", "2024-05-01T10:00:03.5000000Z", "2024-05-01T10:00:03.5000000Z"},
		{"timestamp with an empty payload collapses to a blank line", "2024-05-01T10:00:03.5000000Z ", ""},
		// A tool that stamps its own lines (docker logs -t, kubectl
		// --timestamps) leaves two prefixes stacked. Exactly one of them is
		// GitHub's; the other is output, and eating it would be data loss.
		{"only the first of two stacked timestamps is GitHub's", "2024-05-01T10:00:03.5000000Z 2024-05-01T10:00:03.6000000Z container said hi", "2024-05-01T10:00:03.6000000Z container said hi"},
		{"each line is judged on its own", "2024-05-01T10:00:03.5000000Z stamped\nunstamped\n2024-05-01T10:00:04Z stamped too", "stamped\nunstamped\nstamped too"},
		{"trailing newline survives", "2024-05-01T10:00:03.5000000Z last\n", "last\n"},
		{"blank lines between entries survive", "2024-05-01T10:00:03.5000000Z a\n\n2024-05-01T10:00:04.0000000Z b", "a\n\nb"},
		{"lone newline", "\n", "\n"},
		{"CRLF keeps its carriage return", "2024-05-01T10:00:03.5000000Z a\r\n2024-05-01T10:00:04.0000000Z b\r\n", "a\r\nb\r\n"},
		{"##[error] marker is left for the extractor to find", "2024-05-01T10:00:04.0000000Z ##[error]Process completed with exit code 1.", "##[error]Process completed with exit code 1."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripTimestamps(tc.raw); got != tc.want {
				t.Errorf("StripTimestamps(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestStripTimestampsWholeLog runs the real fixture through the whole-log path
// the monitor uses, against the byte-exact log a reader should get back. The
// expectation is written out rather than derived: asking whether any line "still
// matches tsPrefix" only ever means "by whatever definition the code currently
// holds", and a substring check for a marker cannot fail unless the line is
// deleted outright — an unstripped line contains its marker too. Spelled out,
// the literal pins the group/error markers Parse and Extract key on, the
// indentation of the stack line, the embedded tabs, and the trailing newline all
// at once. A second pass is a no-op — every prefix is already gone — which is
// what makes it safe for a caller not to track whether stripping has happened.
func TestStripTimestampsWholeLog(t *testing.T) {
	raw := readFixture(t, "job_failure.log")
	got := StripTimestamps(raw)

	want := strings.Join([]string{
		"##[group]Run actions/checkout@v4",
		"with:",
		"  repository: owner/repo",
		"##[endgroup]",
		"Syncing repository",
		"##[group]Run go test ./...",
		"go test ./...",
		"shell: /usr/bin/bash -e {0}",
		"##[endgroup]",
		"ok   example/pkg/a  0.012s",
		"--- FAIL: TestThing (0.00s)",
		"    thing_test.go:42: expected 1 got 2",
		"FAIL",
		"FAIL\texample/pkg/b\t0.020s",
		"##[error]Process completed with exit code 1.",
		"##[group]Run actions/upload-artifact@v4",
		"##[endgroup]",
		"artifact uploaded",
		"", // the fixture's trailing newline
	}, "\n")
	if got != want {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
		if len(gotLines) != len(wantLines) {
			t.Fatalf("line count changed: %d lines in, %d out, want %d", len(strings.Split(raw, "\n")), len(gotLines), len(wantLines))
		}
		for i := range wantLines {
			if gotLines[i] != wantLines[i] {
				t.Errorf("line %d = %q, want %q", i, gotLines[i], wantLines[i])
			}
		}
	}
	if again := StripTimestamps(got); again != got {
		t.Errorf("second pass over an already-stripped log changed it:\n%q\n%q", got, again)
	}
}

func TestFullCommand(t *testing.T) {
	raw := strings.Join([]string{
		`2024-05-01T10:00:00.0000000Z ##[group]Run echo "first"`,
		`2024-05-01T10:00:00.0000001Z echo "first"`,
		`2024-05-01T10:00:00.0000002Z echo "second" >&2`,
		`2024-05-01T10:00:00.0000003Z exit 1`,
		`2024-05-01T10:00:00.0000004Z shell: /usr/bin/bash -e {0}`,
		`2024-05-01T10:00:00.0000005Z env:`,
		`2024-05-01T10:00:00.0000006Z   FOO: bar`,
		`2024-05-01T10:00:00.0000007Z ##[endgroup]`,
		`2024-05-01T10:00:01.0000000Z boom`,
		`2024-05-01T10:00:02.0000000Z ##[group]Run actions/checkout@v4`,
		`2024-05-01T10:00:02.0000001Z with:`,
		`2024-05-01T10:00:02.0000002Z   repository: owner/repo`,
		`2024-05-01T10:00:02.0000003Z env:`,
		`2024-05-01T10:00:02.0000004Z   TOKEN: ***`,
		`2024-05-01T10:00:02.0000005Z ##[endgroup]`,
		`2024-05-01T10:00:03.0000000Z ##[group]Run actions/upload-artifact@v4`,
		`2024-05-01T10:00:03.0000001Z ##[endgroup]`,
		"",
	}, "\n")
	secs := Parse(raw)
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3", len(secs))
	}

	run := secs[0]
	if got, want := run.Command(), `echo "first"`; got != want {
		t.Errorf("Command() = %q, want %q (header only)", got, want)
	}
	wantFull := "echo \"first\"\necho \"second\" >&2\nexit 1"
	if got := run.FullCommand(); got != wantFull {
		t.Errorf("FullCommand() = %q, want %q", got, wantFull)
	}

	// An action shows its ref plus the echoed with:/env: it was called with.
	wantAction := "actions/checkout@v4\nwith:\n  repository: owner/repo\nenv:\n  TOKEN: ***"
	if got := secs[1].FullCommand(); got != wantAction {
		t.Errorf("action FullCommand() = %q, want %q", got, wantAction)
	}
	// An action with no echoed inputs falls back to the bare ref.
	if got := secs[2].FullCommand(); got != "actions/upload-artifact@v4" {
		t.Errorf("action-no-inputs FullCommand() = %q", got)
	}
}

func TestClampCommand(t *testing.T) {
	five := "a\nb\nc\nd\ne"
	cases := []struct {
		name     string
		cmd      string
		maxLines int
		want     string
	}{
		{"unlimited zero", five, 0, five},
		{"unlimited negative", five, -1, five},
		{"under limit", five, 8, five},
		{"exactly limit", five, 5, five},
		{"over limit", five, 3, "a\nb\nc\n… (2 more lines) …"},
		{"empty", "", 3, ""},
		{"single line under", "only", 3, "only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampCommand(tc.cmd, tc.maxLines); got != tc.want {
				t.Errorf("ClampCommand(%q, %d) = %q, want %q", tc.cmd, tc.maxLines, got, tc.want)
			}
		})
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}
