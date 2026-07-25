package logs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// FuzzParse exercises the job-log splitter with arbitrary bytes. Parse must
// never panic, each section's Header must be a single line, and the Section
// accessors derived from fuzzed input must themselves be panic-free.
func FuzzParse(f *testing.F) {
	f.Add("")
	f.Add("plain line with no markers\n")
	f.Add("##[group]Run go test ./...\n##[endgroup]\n--- FAIL\n##[error]boom\n")
	f.Add("2024-05-01T10:00:00.0000000Z ##[group]Run actions/checkout@v4\n2024-05-01T10:00:00.0000001Z ##[endgroup]\nhi\n")
	if data, err := os.ReadFile(filepath.Join("testdata", "job_failure.log")); err == nil {
		f.Add(string(data))
	}

	f.Fuzz(func(t *testing.T, raw string) {
		secs := Parse(raw)
		for _, s := range secs {
			// The header is the text of a single ##[group] line, so it must
			// never span lines.
			if strings.Contains(s.Header, "\n") {
				t.Fatalf("header spans multiple lines: %q", s.Header)
			}
			// The accessors must not panic on any parsed section.
			_ = s.Command()
			_ = s.Kind()
			_ = ClampCommand(s.FullCommand(), DefaultMaxCommandLines)
		}
		// ErrorSections must be a subset that all carry an error marker.
		for _, s := range ErrorSections(secs) {
			if !s.HasError {
				t.Fatalf("ErrorSections returned a section without HasError: %q", s.Header)
			}
		}
	})
}

// FuzzStripTimestamps holds StripTimestamps to a line-by-line oracle written
// independently of the production regexp (githubStampLen): every line must come
// back as itself minus exactly the GitHub stamp it opens with — no more, and no
// less. Both halves matter. The monitor hands the result straight to a reader as
// a whole log, so a byte eaten off the wrong end is an invisible lie about what
// CI printed; and an oracle that only forbade over-removal would sit green over
// a strip that had quietly stopped stripping.
func FuzzStripTimestamps(f *testing.F) {
	f.Add("")
	f.Add("plain line with no timestamp\n")
	f.Add("2024-05-01T10:00:00.0000000Z hello\n2024-05-01T10:00:01Z world\n")
	f.Add("2024-05-01T10:00:00.0000000Z 2024-05-01T10:00:00.0000000Z stacked\n")
	f.Add("2024-05-01T10:00:00.0000000Z \n\n2024-05-01T10:00:01Z a\r\n")
	// Near-misses the seed corpus has to carry, since active fuzzing does not
	// gate CI: a payload-owned tab where the separator space would be, an
	// indented stamp, loose field widths, and a multibyte payload.
	f.Add("2024-05-01T10:00:00.0000000Z\ttab-separated\n  2024-05-01T10:00:01Z indented\n")
	f.Add("1-2-3T4:5:6Z loose\n2024-05-01T10:00:00.0000000Z ✗ échec — 你好 🙂\n")
	if data, err := os.ReadFile(filepath.Join("testdata", "job_failure.log")); err == nil {
		f.Add(string(data))
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got := StripTimestamps(raw)

		in, out := strings.Split(raw, "\n"), strings.Split(got, "\n")
		if len(in) != len(out) {
			t.Fatalf("line count changed: %d -> %d", len(in), len(out))
		}
		for i := range in {
			want := in[i][githubStampLen(in[i]):]
			if out[i] != want {
				t.Fatalf("line %d: %q -> %q, want %q", i, in[i], out[i], want)
			}
		}
		// Idempotent once no line still opens with a timestamp — the state every
		// GitHub log reaches in one pass. Stacked timestamps are the deliberate
		// exception: the second one is the tool's own output, not GitHub's, so a
		// re-run would strip content and is not asserted to be a no-op.
		if slices.ContainsFunc(out, func(l string) bool { return githubStampLen(l) > 0 }) {
			return
		}
		if again := StripTimestamps(got); again != got {
			t.Fatalf("not a fixpoint: %q -> %q", got, again)
		}
	})
}

// FuzzExtract checks the excerpt builder. Extract must never panic and must
// only emit content lines that came from the input (or synthetic "… omitted …"
// markers): it selects and elides, it never fabricates content. Blank lines are
// ignored — Extract may introduce layout whitespace around the markers.
func FuzzExtract(f *testing.F) {
	f.Add("a\nb\nc", 100, 10, 100)
	f.Add(strings.Repeat("clean\n", 300)+"fatal: boom\n", 100, 2, 50)
	f.Add("", 1, 1, 1)

	f.Fuzz(func(t *testing.T, raw string, short, ctx, tail int) {
		// Clamp the knobs to sane, small ranges so the fuzzer can't request a
		// pathological allocation; the logic under test is unaffected.
		clamp := func(v, lo, hi int) int {
			if v < lo {
				return lo
			}
			if v > hi {
				return hi
			}
			return v
		}
		lines := strings.Split(raw, "\n")
		opts := Options{
			ShortThreshold: clamp(short, 0, 1000),
			Context:        clamp(ctx, 0, 100),
			Tail:           clamp(tail, 0, 1000),
			Pattern:        DefaultPattern(),
		}

		got := Extract(lines, opts)
		if got == "" {
			return
		}

		input := make(map[string]struct{}, len(lines))
		for _, l := range lines {
			input[l] = struct{}{}
		}
		for l := range strings.SplitSeq(got, "\n") {
			if l == "" {
				continue // layout whitespace, not fabricated content
			}
			if _, ok := input[l]; ok {
				continue
			}
			if strings.HasPrefix(l, "… (") && strings.HasSuffix(l, ") …") {
				continue // synthetic omission marker
			}
			t.Fatalf("Extract emitted a line absent from the input: %q", l)
		}
	})
}

// FuzzClampCommand asserts the truncation contract: a non-positive limit is a
// no-op, and when truncation happens the result is exactly maxLines kept lines
// plus a single trailing marker.
func FuzzClampCommand(f *testing.F) {
	f.Add("a\nb\nc\nd\ne", 3)
	f.Add("", 5)
	f.Add("single", 0)

	f.Fuzz(func(t *testing.T, cmd string, maxLines int) {
		if maxLines > 10000 {
			maxLines = 10000 // keep the fuzzer from requesting a giant join
		}
		got := ClampCommand(cmd, maxLines)

		if maxLines <= 0 || cmd == "" {
			if got != cmd {
				t.Fatalf("non-positive/empty limit must be a no-op: got %q want %q", got, cmd)
			}
			return
		}

		in := strings.Split(cmd, "\n")
		if len(in) <= maxLines {
			if got != cmd {
				t.Fatalf("input within limit must be unchanged: got %q want %q", got, cmd)
			}
			return
		}

		out := strings.Split(got, "\n")
		if len(out) != maxLines+1 {
			t.Fatalf("truncated output should be maxLines+1 lines: got %d want %d", len(out), maxLines+1)
		}
		if last := out[len(out)-1]; !strings.Contains(last, "more lines") {
			t.Fatalf("missing truncation marker, last line = %q", last)
		}
	})
}
