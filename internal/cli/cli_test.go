package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

const failLog = `2024-05-01T10:00:00.0000000Z ##[group]Run actions/checkout@v4
2024-05-01T10:00:00.0000001Z ##[endgroup]
2024-05-01T10:00:01.0000000Z Synced
2024-05-01T10:00:02.0000000Z ##[group]Run go test ./...
2024-05-01T10:00:02.0000001Z go test ./...
2024-05-01T10:00:02.0000002Z ##[endgroup]
2024-05-01T10:00:03.0000000Z --- FAIL: TestThing
2024-05-01T10:00:04.0000000Z ##[error]Process completed with exit code 1.
`

func TestBuildFailedStepsAssociatesErrorSection(t *testing.T) {
	a := &app{opts: logs.DefaultOptions()}
	job := model.JobResult{Steps: []model.StepOverview{
		{Number: 1, Name: "Checkout", Conclusion: "success"},
		{Number: 2, Name: "Run tests", Conclusion: "failure"},
	}}
	fs := a.buildFailedSteps(job, failLog)
	if len(fs) != 1 {
		t.Fatalf("got %d failed steps, want 1: %+v", len(fs), fs)
	}
	if fs[0].Name != "Run tests" || fs[0].Number != 2 {
		t.Errorf("step name/number = %q/%d", fs[0].Name, fs[0].Number)
	}
	if fs[0].Command != "go test ./..." || fs[0].Kind != model.KindBash {
		t.Errorf("command/kind = %q/%q", fs[0].Command, fs[0].Kind)
	}
	if !strings.Contains(fs[0].Excerpt, "--- FAIL: TestThing") {
		t.Errorf("excerpt missing failure: %q", fs[0].Excerpt)
	}
}

func TestBuildFailedStepsFallbackNoErrorMarker(t *testing.T) {
	a := &app{opts: logs.DefaultOptions()}
	raw := "2024-05-01T10:00:00.0000000Z ##[group]Run go build\n2024-05-01T10:00:00.0000001Z ##[endgroup]\n2024-05-01T10:00:01.0000000Z some output\n"
	job := model.JobResult{Steps: []model.StepOverview{{Number: 1, Name: "Build", Conclusion: "failure"}}}
	fs := a.buildFailedSteps(job, raw)
	if len(fs) != 1 {
		t.Fatalf("got %d failed steps, want 1", len(fs))
	}
	if fs[0].Name != "Build" {
		t.Errorf("fallback should use the failed step name, got %q", fs[0].Name)
	}
	if !strings.Contains(fs[0].Excerpt, "some output") {
		t.Errorf("fallback excerpt should contain the log body: %q", fs[0].Excerpt)
	}
}

func TestResolveToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if _, err := resolveToken(""); err == nil {
		t.Errorf("expected error when no token is set")
	}
	if got, _ := resolveToken("flagtok"); got != "flagtok" {
		t.Errorf("flag token should win, got %q", got)
	}
	t.Setenv("GH_TOKEN", "envtok")
	if got, _ := resolveToken(""); got != "envtok" {
		t.Errorf("GH_TOKEN fallback failed, got %q", got)
	}
}

func TestBuildExtractOptionsFull(t *testing.T) {
	opts, err := buildExtractOptions(options{shortThreshold: 100, context: 10, tail: 100, full: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.ShortThreshold <= 100 {
		t.Errorf("--full should raise the short threshold, got %d", opts.ShortThreshold)
	}

	if _, err := buildExtractOptions(options{pattern: "("}); err == nil {
		t.Errorf("expected error for invalid regexp")
	}
}

func TestRunVersionFlag(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := Run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--version exit code = %d, want 0", code)
	}
	if got := stdout.String(); !strings.HasPrefix(got, "shuck ") {
		t.Errorf("--version output = %q, want it to start with %q", got, "shuck ")
	}
	if stderr.Len() != 0 {
		t.Errorf("--version wrote to stderr: %q", stderr.String())
	}
}

func TestVersionString(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v9.9.9"
	if got := versionString(); got != "v9.9.9" {
		t.Errorf("versionString() = %q, want injected value %q", got, "v9.9.9")
	}

	version = ""
	if got := versionString(); got == "" {
		t.Errorf("versionString() returned empty with no injected version")
	}
}

func TestBuildExtractOptionsRejectsNegative(t *testing.T) {
	cases := map[string]options{
		"context":         {context: -1},
		"short-threshold": {shortThreshold: -1},
		"tail":            {tail: -1},
	}
	for name, o := range cases {
		if _, err := buildExtractOptions(o); err == nil {
			t.Errorf("negative %s should be rejected", name)
		}
	}
}

func TestPermuteArgs(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantBool bool
		wantStr  string
		wantArgs []string
	}{
		{"flags already first", []string{"--flag", "--str", "v", "a", "b"}, true, "v", []string{"a", "b"}},
		{"bool flag after positionals", []string{"a", "b", "--flag"}, true, "", []string{"a", "b"}},
		{"value flag after positionals", []string{"a", "--str", "v"}, false, "v", []string{"a"}},
		{"combined value flag", []string{"a", "--str=v"}, false, "v", []string{"a"}},
		{"double dash keeps the rest positional", []string{"a", "--", "--flag"}, false, "", []string{"a", "--flag"}},
		{"no args", nil, false, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			var b bool
			var s string
			fs.BoolVar(&b, "flag", false, "")
			fs.StringVar(&s, "str", "", "")
			if err := fs.Parse(permuteArgs(fs, c.in)); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if b != c.wantBool {
				t.Errorf("bool flag = %v, want %v", b, c.wantBool)
			}
			if s != c.wantStr {
				t.Errorf("str flag = %q, want %q", s, c.wantStr)
			}
			got := fs.Args()
			same := len(got) == len(c.wantArgs)
			for i := 0; same && i < len(got); i++ {
				same = got[i] == c.wantArgs[i]
			}
			if !same {
				t.Errorf("args = %v, want %v", got, c.wantArgs)
			}
		})
	}
}

// TestRunJSONOfflineFlagsAfterTarget exercises the full --json path through the
// cache (no network) and, by placing flags after the positionals, also proves
// the arg-permutation pre-pass.
func TestRunJSONOfflineFlagsAfterTarget(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	report := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix", HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			ID: 1, Name: "build", Conclusion: "failure", Inspected: true,
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "boom"}},
		}},
	}
	if err := cache.Save(report); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"--offline", "o/r", "42", "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (failures present); stderr=%q", code, stderr.String())
	}

	var doc jsonout.Document
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	if doc.SchemaVersion != jsonout.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, jsonout.SchemaVersion)
	}
	if doc.Summary.Failed != 1 {
		t.Errorf("summary.failed = %d, want 1", doc.Summary.Failed)
	}
	if len(doc.FailedJobs) != 1 || len(doc.FailedJobs[0].FailedSteps) != 1 ||
		doc.FailedJobs[0].FailedSteps[0].Name != "Run tests" {
		t.Errorf("unexpected failed jobs: %+v", doc.FailedJobs)
	}
}

func TestExitFor(t *testing.T) {
	if exitFor(&model.Report{}) != 0 {
		t.Errorf("clean report should exit 0")
	}
	withFail := &model.Report{FailedJobs: []model.JobResult{{ID: 1}}}
	if exitFor(withFail) != 1 {
		t.Errorf("report with failures should exit 1")
	}
}

func TestCanonicalDashes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"--full", "--full"},
		{"-full", "-full"},
		{"—full", "--full"}, // em dash (U+2014) — macOS smart-dash "--"
		{"–full", "--full"}, // en dash (U+2013)
		{"―full", "--full"}, // horizontal bar (U+2015)
		{"—context=5", "--context=5"},
		{"—no-cache", "--no-cache"}, // inner ASCII hyphen is untouched
		{"−full", "-full"},          // minus sign (U+2212) → single hyphen
		{"‐full", "-full"},          // hyphen (U+2010)
		{"‑full", "-full"},          // non-breaking hyphen (U+2011)
		{"‒full", "-full"},          // figure dash (U+2012)
		{"—", "--"},                 // lone em dash stays the "--" separator
		{"42", "42"},                // positionals are untouched
		{"o/r", "o/r"},
		{"https://github.com/o/r/pull/42", "https://github.com/o/r/pull/42"},
		{"", ""},
	}
	for _, c := range cases {
		if got := canonicalDashes(c.in); got != c.want {
			t.Errorf("canonicalDashes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseArgsReportedEmDashBug pins the exact reported regression:
// "shuck 42 —full" (em dash, flag after a bare PR number) used to be read as two
// positionals and fail with "invalid repo \"42\"".
func TestParseArgsReportedEmDashBug(t *testing.T) {
	o, pos, err := parseArgs([]string{"42", "—full"}, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !o.full {
		t.Errorf("--full (em dash) not parsed: %+v", o)
	}
	if len(pos) != 1 || pos[0] != "42" {
		t.Errorf("positionals = %v, want [42]", pos)
	}
}

// TestParseArgsFlagTargetMatrix proves every flag parses correctly with every
// target form, in any flag ordering, whether the flag is written with ASCII
// hyphens or a Unicode em/en dash. In all cases the flag must be set and the
// leftover positionals must equal exactly the target tokens.
func TestParseArgsFlagTargetMatrix(t *testing.T) {
	type flagSpec struct {
		name  string
		toks  []string
		check func(options) bool
	}
	flagSpecs := []flagSpec{
		{"full", []string{"--full"}, func(o options) bool { return o.full }},
		{"json", []string{"--json"}, func(o options) bool { return o.json }},
		{"refresh", []string{"--refresh"}, func(o options) bool { return o.refresh }},
		{"no-cache", []string{"--no-cache"}, func(o options) bool { return o.noCache }},
		{"offline", []string{"--offline"}, func(o options) bool { return o.offline }},
		{"context-space", []string{"--context", "5"}, func(o options) bool { return o.context == 5 }},
		{"context-equals", []string{"--context=5"}, func(o options) bool { return o.context == 5 }},
		{"short-threshold", []string{"--short-threshold", "7"}, func(o options) bool { return o.shortThreshold == 7 }},
		{"tail", []string{"--tail", "3"}, func(o options) bool { return o.tail == 3 }},
		{"pattern", []string{"--pattern", "boom"}, func(o options) bool { return o.pattern == "boom" }},
		{"token", []string{"--token", "tok"}, func(o options) bool { return o.token == "tok" }},
	}
	targets := [][]string{
		{"o/r", "42"},
		{"https://github.com/o/r/pull/42"},
		{"https://github.com/o/r/actions/runs/123"},
		{"https://github.com/o/r/actions/runs/123/job/456"},
		{"42"},
		{},
	}
	dashes := []struct {
		name string
		conv func(string) string
	}{
		{"ascii", func(s string) string { return s }},
		{"emdash", func(s string) string { return replaceLeadingDashes(s, "—") }},
		{"endash", func(s string) string { return replaceLeadingDashes(s, "–") }},
	}
	orderings := []string{"flags-first", "flags-last", "flags-between"}

	for _, fsp := range flagSpecs {
		for _, tgt := range targets {
			for _, d := range dashes {
				for _, ord := range orderings {
					if ord == "flags-between" && len(tgt) < 2 {
						continue
					}
					ftoks := make([]string, len(fsp.toks))
					for i, tk := range fsp.toks {
						ftoks[i] = d.conv(tk)
					}
					var args []string
					switch ord {
					case "flags-first":
						args = append(append([]string{}, ftoks...), tgt...)
					case "flags-last":
						args = append(append([]string{}, tgt...), ftoks...)
					case "flags-between":
						args = append(args, tgt[0])
						args = append(args, ftoks...)
						args = append(args, tgt[1:]...)
					}
					name := fmt.Sprintf("%s/%s/%s/%d-tgt", fsp.name, d.name, ord, len(tgt))
					t.Run(name, func(t *testing.T) {
						o, pos, err := parseArgs(args, io.Discard)
						if err != nil {
							t.Fatalf("parseArgs(%q): %v", args, err)
						}
						if !fsp.check(o) {
							t.Errorf("parseArgs(%q): flag %s not set: %+v", args, fsp.name, o)
						}
						if !equalStrings(pos, tgt) {
							t.Errorf("parseArgs(%q): positionals = %v, want %v", args, pos, tgt)
						}
					})
				}
			}
		}
	}
}

// TestParseArgsThenResolveRouting checks that a flag (with a Unicode dash)
// alongside each fully-qualified target form leaves positionals that target
// resolution routes to the right PR / run / job.
func TestParseArgsThenResolveRouting(t *testing.T) {
	cases := []struct {
		args         []string
		owner, repo  string
		number       int
		runID, jobID int64
	}{
		{[]string{"o/r", "42", "—json"}, "o", "r", 42, 0, 0},
		{[]string{"—full", "https://github.com/o/r/pull/42"}, "o", "r", 42, 0, 0},
		{[]string{"https://github.com/o/r/actions/runs/123", "—json"}, "o", "r", 0, 123, 0},
		{[]string{"https://github.com/o/r/actions/runs/123/job/456", "—full"}, "o", "r", 0, 123, 456},
	}
	for _, c := range cases {
		_, pos, err := parseArgs(c.args, io.Discard)
		if err != nil {
			t.Fatalf("parseArgs(%q): %v", c.args, err)
		}
		tgt, err := target.Resolve(pos)
		if err != nil {
			t.Fatalf("Resolve(%v) from %q: %v", pos, c.args, err)
		}
		if tgt.Owner != c.owner || tgt.Repo != c.repo || tgt.Number != c.number ||
			tgt.RunID != c.runID || tgt.JobID != c.jobID {
			t.Errorf("Resolve(%v) = %+v, want owner=%s repo=%s num=%d run=%d job=%d",
				pos, tgt, c.owner, c.repo, c.number, c.runID, c.jobID)
		}
	}
}

// TestRunUnicodeDashFlagsEndToEnd drives the full Run path, including the
// reported shape (slug + number first, flags after) written with em dashes,
// against a seeded offline cache so no network is needed.
func TestRunUnicodeDashFlagsEndToEnd(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	report := &model.Report{
		PR: model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix", HeadSHA: "abc1234"},
		FailedJobs: []model.JobResult{{
			ID: 1, Name: "build", Conclusion: "failure", Inspected: true,
			FailedSteps: []model.FailedStep{{Number: 2, Name: "Run tests", Excerpt: "boom"}},
		}},
	}
	if err := cache.Save(report); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"o/r", "42", "—offline", "—json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (failures present); stderr=%q", code, stderr.String())
	}

	var doc jsonout.Document
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	if doc.Summary.Failed != 1 {
		t.Errorf("summary.failed = %d, want 1", doc.Summary.Failed)
	}
}

// replaceLeadingDashes rewrites a token's leading "--" into the given (Unicode)
// dash so tests can simulate macOS smart-dash / rich-text mangling. Value tokens
// (which don't start with "--") pass through unchanged.
func replaceLeadingDashes(tok, dash string) string {
	if strings.HasPrefix(tok, "--") {
		return dash + tok[len("--"):]
	}
	return tok
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
