package distil

import (
	"strings"
	"unicode/utf8"
)

// DefaultSummaryLimit is the default byte budget for a delivered summary —
// small enough for a channel notification and far under the gateway's
// buffer-row ceiling, large enough for the header plus the first failing
// steps' error headlines.
const DefaultSummaryLimit = 16 << 10

// CapSummary enforces a byte budget on an agent-ready summary. A summary
// within limit (or a limit <= 0, meaning unlimited) is returned unchanged
// with truncated=false. Otherwise the result keeps whole lines from the
// start — the header and first failing steps, which carry the first error —
// falling back to a mid-line cut at a UTF-8 rune boundary when not even one
// line fits, then appends note (the caller's truncation marker, e.g. a
// pointer to the archived full logs) on its own line. The result is always
// at most limit bytes: the note itself is rune-truncated when it alone
// exceeds the budget.
func CapSummary(summary string, limit int, note string) (string, bool) {
	if limit <= 0 || len(summary) <= limit {
		return summary, false
	}

	suffix := ""
	if note != "" {
		suffix = "\n" + note
	}
	budget := limit - len(suffix)
	if budget <= 0 {
		// The note alone (over)fills the budget; the marker matters more
		// than the content it annotates.
		return cutRunes(note, limit), true
	}

	head := cutRunes(summary, budget)
	if p := strings.LastIndexByte(head, '\n'); p > 0 {
		head = head[:p]
	}
	return head + suffix, true
}

// cutRunes truncates s to at most n bytes without splitting a UTF-8 rune.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
