package logs

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// DefaultPattern matches the variations of error/failure tokens we grep for.
func DefaultPattern() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(error|fail(ed|ure)?|fatal|panic|exception|\berr\b|##\[error\])`)
}

// DefaultNoisePattern matches the lines a build or test runner prints about
// work that *succeeded*: go's per-package `ok`, jest's ticks, pytest's
// `PASSED`, cargo's `Compiling`. In a failing job these are the bulk of the
// log and not one of them helps you fix anything — a `go test ./...` run over
// twenty packages spends nineteen lines saying so before the one line that
// matters.
//
// They are cut even from a log short enough to be returned whole, because
// "short" is a budget for how much an agent has to read, and spending it on
// packages that passed is the noise this exists to remove.
//
// The pattern is deliberately anchored: a token like `ok` or `PASS` means
// "this unit succeeded" only at the head of a line, and matching it mid-line
// would start eating prose. Nothing here is ever trusted on its own — see
// collapseNoise, where a line matching the failure pattern is kept whatever
// this says about it.
func DefaultNoisePattern() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*(` + strings.Join([]string{
		`ok\s+\S+`,                     // go test:  ok   example.com/pkg  0.01s
		`\?\s+\S+\s+\[no test files\]`, // go test:  ?    example.com/pkg  [no test files]
		`---\s*PASS:`,                  // go test -v
		`===\s*(RUN|CONT|PAUSE)\b`,     // go test -v scaffolding
		`PASS\s+\S`,                    // jest:     PASS  src/foo.test.ts
		`[✓✔√]\s`,                      // jest / vitest / mocha per-test ticks
		`\S+::\S+\s+PASSED`,            // pytest -v
		`test\s+\S+\s+\.\.\.\s*ok\b`,   // cargo test
		`(Compiling|Downloading|Downloaded|Fresh|Updating)\s+\S`, // cargo progress
		`added\s+\d+\s+packages`,                                 // npm install
	}, "|") + `)`)
}

// Options controls how much of a section's output to surface.
type Options struct {
	ShortThreshold int            // logs with <= this many lines are returned whole
	Context        int            // lines of context kept on each side of a match
	Tail           int            // lines tailed when a long log has no matches
	Pattern        *regexp.Regexp // error matcher; nil means DefaultPattern
	// Noise matches lines reporting success or progress, which are collapsed
	// away when the log has failure signal to show instead. nil disables the
	// collapse entirely — which is what `--full` wants, and what keeps an
	// Options built by hand behaving exactly as it did before this existed.
	Noise *regexp.Regexp
}

// DefaultOptions returns the documented defaults.
func DefaultOptions() Options {
	return Options{
		ShortThreshold: 100,
		Context:        10,
		Tail:           100,
		Pattern:        DefaultPattern(),
		Noise:          DefaultNoisePattern(),
	}
}

// Extract reduces a section's output to the high-signal excerpt:
//   - runs of success/progress lines are collapsed, if there is failure signal
//     to keep instead;
//   - short logs are then returned whole;
//   - long logs are grepped, keeping ±Context lines around each match;
//   - long logs with no match are tailed to the last Tail lines.
func Extract(lines []string, opts Options) string {
	if opts.Pattern == nil {
		opts.Pattern = DefaultPattern()
	}
	lines = trimBlankEdges(lines)
	if len(lines) == 0 {
		return ""
	}
	// Only collapse when something failed. A log with no failure signal at all
	// is one shuck has not understood — a cancelled job, an unfamiliar runner —
	// and thinning it on the strength of a guess about which lines are boring
	// would take away the only material anyone has to read.
	if opts.Noise != nil && matches(lines, opts.Pattern) {
		lines = collapseNoise(lines, opts.Noise, opts.Pattern)
	}
	if len(lines) <= opts.ShortThreshold {
		return strings.Join(lines, "\n")
	}

	var hits []int
	for i, l := range lines {
		if opts.Pattern.MatchString(l) {
			hits = append(hits, i)
		}
	}

	if len(hits) == 0 {
		start := max(len(lines)-opts.Tail, 0)
		omitted := start
		out := lines[start:]
		if omitted > 0 {
			return ellipsis(omitted) + "\n" + strings.Join(out, "\n")
		}
		return strings.Join(out, "\n")
	}

	return renderWindows(lines, mergeWindows(hits, opts.Context, len(lines)))
}

type window struct{ start, end int } // [start, end)

func mergeWindows(hits []int, ctx, n int) []window {
	var ws []window
	for _, h := range hits {
		s := max(h-ctx, 0)
		e := min(h+ctx+1, n)
		if len(ws) > 0 && s <= ws[len(ws)-1].end {
			if e > ws[len(ws)-1].end {
				ws[len(ws)-1].end = e
			}
			continue
		}
		ws = append(ws, window{s, e})
	}
	return ws
}

func renderWindows(lines []string, ws []window) string {
	var b strings.Builder
	prevEnd := 0
	for i, w := range ws {
		if gap := w.start - prevEnd; gap > 0 {
			b.WriteString(ellipsis(gap))
			b.WriteByte('\n')
		} else if i == 0 && w.start > 0 {
			b.WriteString(ellipsis(w.start))
			b.WriteByte('\n')
		}
		b.WriteString(strings.Join(lines[w.start:w.end], "\n"))
		b.WriteByte('\n')
		prevEnd = w.end
	}
	if tail := len(lines) - prevEnd; tail > 0 {
		b.WriteString(ellipsis(tail))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// matches reports whether any line matches re.
func matches(lines []string, re *regexp.Regexp) bool {
	return slices.ContainsFunc(lines, re.MatchString)
}

// collapseNoise replaces each run of success/progress lines with one line
// saying how many went, so the excerpt keeps the shape of the log — where the
// failure sits relative to the work around it — without paying a line per
// package that passed.
//
// signal always wins. A noise pattern is a heuristic about somebody else's
// output format, and the cost of it being wrong has to fall on brevity rather
// than on correctness: a line that looks like success but mentions a failure
// is kept, every time. That is what makes the pattern safe to extend without
// re-auditing every runner it might touch.
func collapseNoise(lines []string, noise, signal *regexp.Regexp) []string {
	out := make([]string, 0, len(lines))
	run := 0
	flush := func() {
		if run > 0 {
			out = append(out, quietEllipsis(run))
			run = 0
		}
	}
	for _, l := range lines {
		if noise.MatchString(l) && !signal.MatchString(l) {
			run++
			continue
		}
		flush()
		out = append(out, l)
	}
	flush()
	return out
}

func ellipsis(n int) string {
	return fmt.Sprintf("… (%d lines omitted) …", n)
}

// quietEllipsis names what was dropped, so a collapsed run reads as a
// deliberate cut and not as a log that happens to be missing its middle.
func quietEllipsis(n int) string {
	if n == 1 {
		return "… (1 passing/progress line omitted) …"
	}
	return fmt.Sprintf("… (%d passing/progress lines omitted) …", n)
}

func trimBlankEdges(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}
