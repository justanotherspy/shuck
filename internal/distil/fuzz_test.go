package distil

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// Mirrors of unexported markers the fuzz invariants must recognize.
var (
	fuzzTSPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)
	fuzzEllipsis = regexp.MustCompile(`^… \(\d+ lines omitted\) …$`)
)

var fuzzConclusions = []string{"failure", "cancelled", "timed_out", "success", ""}

// FuzzDistilCIFailure asserts CIFailure's semantic contract on arbitrary log
// bytes (CI logs are attacker-influenceable via PR contents): it never
// panics, is deterministic, always yields at least one step within the
// pairing bound, only ever excerpts lines that exist in the input (or its
// own omission markers), and summarizes every non-empty result.
func FuzzDistilCIFailure(f *testing.F) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", e.Name(), "log.txt"))
		if err != nil {
			f.Fatalf("read seed log: %v", err)
		}
		f.Add(string(raw), byte(0), 3, 100, 10, 100, 30)
	}
	f.Add("##[group]Run go test ./...\n##[endgroup]\n--- FAIL: TestX\n##[error]exit 1\n", byte(1), 4, 1, 0, 1, 1)
	f.Add("no groups at all\njust noise\n", byte(2), 0, 0, 0, 0, 0)

	f.Fuzz(func(t *testing.T, raw string, conclusionSel byte, nSteps, shortThreshold, context, tail, maxCmd int) {
		// Clamp the knobs to sane ranges: negatives are rejected by design and
		// huge values only slow the fuzzer down without exploring new logic.
		clamp := func(v, lo, hi int) int { return min(max(v, lo), hi) }
		nSteps = clamp(nSteps, 0, 64)
		opts := DefaultOptions()
		opts.Extract.ShortThreshold = clamp(shortThreshold, 0, 4096)
		opts.Extract.Context = clamp(context, 0, 4096)
		opts.Extract.Tail = clamp(tail, 0, 4096)
		opts.MaxCommandLines = clamp(maxCmd, 0, 4096)

		conclusion := fuzzConclusions[int(conclusionSel)%len(fuzzConclusions)]
		steps := make([]model.StepOverview, 0, nSteps)
		drillable := 0
		for i := range nSteps {
			c := "success"
			if i%2 == 0 {
				c = "failure"
				drillable++
			}
			steps = append(steps, model.StepOverview{Number: i + 1, Name: fmt.Sprintf("step-%d", i+1), Conclusion: c})
		}

		in := Input{JobName: "fuzz-job", JobConclusion: conclusion, Steps: steps, RawLog: raw, Options: opts}
		res, err := CIFailure(in)
		if err != nil {
			t.Fatalf("valid options must not error: %v", err)
		}
		res2, err := CIFailure(in)
		if err != nil || !reflect.DeepEqual(res, res2) {
			t.Fatalf("CIFailure is not deterministic")
		}

		// Always at least one step; never more than the pairing bound.
		errMarkers := strings.Count(raw, "##[error]")
		bound := max(max(drillable, errMarkers), 1)
		if len(res.FailedSteps) < 1 || len(res.FailedSteps) > bound {
			t.Fatalf("got %d failed steps, want within [1, %d]", len(res.FailedSteps), bound)
		}

		// Excerpt provenance: every line either exists in the timestamp-stripped
		// input, is an omission marker, or is the no-match fallback. (Command is
		// exempt: clamping appends its own "… (N more lines) …" marker.)
		input := make(map[string]bool)
		for line := range strings.SplitSeq(raw, "\n") {
			input[fuzzTSPrefix.ReplaceAllString(line, "")] = true
		}
		for _, fs := range res.FailedSteps {
			if fs.Excerpt == "(no matching error log section found)" || fs.Excerpt == "" {
				continue
			}
			for line := range strings.SplitSeq(fs.Excerpt, "\n") {
				if input[line] || fuzzEllipsis.MatchString(line) {
					continue
				}
				t.Fatalf("excerpt line not derived from input: %q", line)
			}
		}

		if res.Summary == "" {
			t.Fatalf("summary must be non-empty when steps exist")
		}
	})
}
