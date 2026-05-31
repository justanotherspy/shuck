package logs

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultPattern matches the variations of error/failure tokens we grep for.
func DefaultPattern() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(error|fail(ed|ure)?|fatal|panic|exception|\berr\b|##\[error\])`)
}

// Options controls how much of a section's output to surface.
type Options struct {
	ShortThreshold int            // logs with <= this many lines are returned whole
	Context        int            // lines of context kept on each side of a match
	Tail           int            // lines tailed when a long log has no matches
	Pattern        *regexp.Regexp // error matcher; nil means DefaultPattern
}

// DefaultOptions returns the documented defaults.
func DefaultOptions() Options {
	return Options{ShortThreshold: 100, Context: 10, Tail: 100, Pattern: DefaultPattern()}
}

// Extract reduces a section's output to the high-signal excerpt:
//   - short logs are returned whole;
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

func ellipsis(n int) string {
	return fmt.Sprintf("… (%d lines omitted) …", n)
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
