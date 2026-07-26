package logs

import (
	"strings"
	"testing"
)

// TestExtractCollapsesPassingLines is the case that prompted the collapse: a
// `go test ./...` failure is short enough to be returned whole and is still
// almost entirely packages that passed. The one assertion that matters is that
// the failure and its message survive intact while the successes go.
func TestExtractCollapsesPassingLines(t *testing.T) {
	lines := []string{
		"go test -race ./...",
		"ok  \texample.com/x/action\t1.03s\tcoverage: 100.0% of statements",
		"ok  \texample.com/x/cache\t1.04s\tcoverage: 88.9% of statements",
		"ok  \texample.com/x/cli\t3.03s\tcoverage: 85.4% of statements",
		"--- FAIL: TestThing (0.00s)",
		"    thing_test.go:11: expected 1 got 2",
		"FAIL",
		"FAIL\texample.com/x/logs\t0.09s",
		"ok  \texample.com/x/model\t1.02s\tcoverage: 100.0% of statements",
		"ok  \texample.com/x/render\t1.06s\tcoverage: 98.1% of statements",
		"make: *** [Makefile:64: test] Error 1",
	}

	got := Extract(lines, DefaultOptions())

	for _, want := range []string{
		"--- FAIL: TestThing (0.00s)",
		"thing_test.go:11: expected 1 got 2",
		"FAIL\texample.com/x/logs\t0.09s",
		"make: *** [Makefile:64: test] Error 1",
		"… (3 passing/progress lines omitted) …",
		"… (2 passing/progress lines omitted) …",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "coverage: 100.0%") {
		t.Errorf("a passing package survived the collapse:\n%s", got)
	}
}

// TestExtractKeepsNoiseLookalikesThatMentionFailure is the safety rule that
// makes the noise pattern extensible: it is a guess about somebody else's
// output format, so when it collides with the failure pattern the failure wins.
// A jest tick naming a test about errors is the everyday version of this.
func TestExtractKeepsNoiseLookalikesThatMentionFailure(t *testing.T) {
	lines := []string{
		"PASS  src/ok.test.ts",
		"✓ returns a value",
		"✓ surfaces an error to the caller",
		"FAIL  src/broken.test.ts",
		"  ● renders › throws",
	}

	got := Extract(lines, DefaultOptions())

	if !strings.Contains(got, "surfaces an error to the caller") {
		t.Errorf("a line matching the failure pattern was collapsed as noise:\n%s", got)
	}
	if strings.Contains(got, "returns a value") {
		t.Errorf("an ordinary passing tick survived:\n%s", got)
	}
}

// TestExtractLeavesLogsWithNoFailureSignalAlone covers the other guard. A log
// shuck has not recognized is the one it must not thin: there is nothing to
// promote in its place, so collapsing would remove the only material anyone has.
func TestExtractLeavesLogsWithNoFailureSignalAlone(t *testing.T) {
	lines := []string{
		"ok  \texample.com/x/a\t0.01s",
		"ok  \texample.com/x/b\t0.02s",
		"all done",
	}

	got := Extract(lines, DefaultOptions())

	if got != strings.Join(lines, "\n") {
		t.Errorf("a log with no failure signal was thinned:\n%s", got)
	}
}

// TestExtractWithoutNoisePatternIsUnchanged is what `--full` relies on, and
// what keeps an Options built by hand behaving as it did before the collapse
// existed.
func TestExtractWithoutNoisePatternIsUnchanged(t *testing.T) {
	lines := []string{
		"ok  \texample.com/x/a\t0.01s",
		"--- FAIL: TestThing (0.00s)",
		"ok  \texample.com/x/b\t0.02s",
	}

	opts := DefaultOptions()
	opts.Noise = nil

	if got := Extract(lines, opts); got != strings.Join(lines, "\n") {
		t.Errorf("a nil Noise pattern still collapsed lines:\n%s", got)
	}
}

// TestDefaultNoisePatternMatchesCommonRunners pins the formats the pattern
// claims to know, and the anchoring that keeps it from eating prose: these
// tokens mean "a unit succeeded" at the head of a line and nothing in the
// middle of one.
func TestDefaultNoisePatternMatchesCommonRunners(t *testing.T) {
	noise := DefaultNoisePattern()

	for _, line := range []string{
		"ok  \texample.com/x/a\t0.01s",
		"?   \texample.com/x/b\t[no test files]",
		"--- PASS: TestThing (0.00s)",
		"=== RUN   TestThing",
		"PASS  src/foo.test.ts",
		"  ✓ does the thing",
		"tests/test_x.py::test_y PASSED",
		"test tests::case_001 ... ok",
		"   Compiling serde v1.0.0",
		"added 412 packages in 9s",
	} {
		if !noise.MatchString(line) {
			t.Errorf("noise pattern missed a known success line: %q", line)
		}
	}

	for _, line := range []string{
		"the build is ok now",
		"we should PASS this through",
		"FAIL\texample.com/x/c\t0.03s",
		"--- FAIL: TestThing (0.00s)",
		"##[error]Process completed with exit code 1.",
		"panic: runtime error: index out of range",
	} {
		if noise.MatchString(line) {
			t.Errorf("noise pattern matched a line it must never drop: %q", line)
		}
	}
}
