package distil

import (
	"fmt"
	"strings"

	"github.com/justanotherspy/shuck/internal/logs"
	"github.com/justanotherspy/shuck/internal/model"
)

// headlineMaxRunes caps a step's one-line excerpt headline in the Summary.
const headlineMaxRunes = 120

// summarize renders the agent-ready summary: a header naming the job, its
// conclusion, and the failed-step count, then one line per failed step with
// its heuristic class and the highest-signal excerpt line. Deterministic —
// it derives only from the input and the distilled steps.
func summarize(in Input, steps []model.FailedStep) string {
	job := in.JobName
	if job == "" {
		job = "job"
	}
	conclusion := in.JobConclusion
	if conclusion == "" {
		conclusion = "failure"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s — %d failed step(s)", job, conclusion, len(steps))
	for _, fs := range steps {
		b.WriteString("\n- ")
		b.WriteString(fs.Name)
		if fs.Number > 0 {
			fmt.Fprintf(&b, " (step %d)", fs.Number)
		}
		if fs.Class != model.ClassUnknown {
			fmt.Fprintf(&b, " [%s]", fs.Class)
		}
		if h := headline(fs.Excerpt); h != "" {
			b.WriteString(": ")
			b.WriteString(h)
		}
	}
	return b.String()
}

// headline picks the step's one-line takeaway from its excerpt: the first
// line matching the error pattern, else the first non-blank line, truncated
// to headlineMaxRunes.
func headline(excerpt string) string {
	pattern := logs.DefaultPattern()
	first := ""
	for line := range strings.SplitSeq(excerpt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if first == "" {
			first = line
		}
		if pattern.MatchString(line) {
			return truncate(line, headlineMaxRunes)
		}
	}
	return truncate(first, headlineMaxRunes)
}

// truncate caps s at n runes, appending an ellipsis when it was longer.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
