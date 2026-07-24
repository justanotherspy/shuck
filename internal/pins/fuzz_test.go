package pins

import (
	"context"
	"io"
	"strings"
	"testing"
)

// FuzzScan exercises the workflow scanner with arbitrary bytes. Scan must never
// panic on input it did not write, every Use it returns must be attributable to
// a real input file and a real (1-based) line, and the whole audit/render
// pipeline built on top of it must survive whatever Scan produces.
func FuzzScan(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte(ciWorkflow))
	f.Add([]byte("steps:\n  - uses: actions/checkout@v4\n"))
	f.Add([]byte("steps:\n  - uses: actions/checkout@" + sha422 + " # v4.2.2\n"))
	f.Add([]byte("steps:\n  - uses: ./local\n  - uses: docker://alpine:3.20\n"))
	f.Add([]byte("steps:\n  - uses:\n      - a\n  - uses:\n"))
	f.Add([]byte("a: &x\n  uses: a/b@v1\nb: *x\n"))
	f.Add([]byte("steps:\n  - uses: a/b@v1\n---\nsteps:\n  - uses: a/c@v2\n"))
	f.Add([]byte("jobs:\n  - [unclosed\n"))
	f.Add([]byte("&a [*a]"))
	f.Add([]byte("uses: uses\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Cap the input so the fuzzer spends its budget on structure rather than
		// on feeding the YAML parser megabytes of noise.
		if len(data) > 8<<10 {
			return
		}

		const name = ".github/workflows/fuzz.yml"
		files := map[string][]byte{name: data}
		uses := Scan(files)

		for _, u := range uses {
			if u.File != name {
				t.Fatalf("Scan attributed a use to an unknown file %q", u.File)
			}
			if u.Line < 1 {
				t.Fatalf("Scan returned a non-positive line %d for %q", u.Line, u.Raw)
			}
			switch u.Kind {
			case UseInvalid:
				if u.Err == "" {
					t.Fatalf("an invalid entry carries no reason: %+v", u)
				}
			case UseRemote, UseLocal, UseDocker:
				if u.Raw == "" {
					t.Fatalf("a %v use has an empty reference: %+v", u.Kind, u)
				}
				// Raw is verbatim source text, so it can never span lines.
				if strings.Contains(u.Raw, "\n") {
					t.Fatalf("a reference spans multiple lines: %q", u.Raw)
				}
			default:
				t.Fatalf("Scan returned an unknown kind %d: %+v", u.Kind, u)
			}
			// Only a remote reference carries a slug, and it must be the text
			// before the "@" of the raw reference.
			if u.Kind == UseRemote && !strings.HasPrefix(u.Raw, u.Slug) {
				t.Fatalf("slug %q is not a prefix of raw %q", u.Slug, u.Raw)
			}
			if u.Kind != UseRemote && (u.Slug != "" || u.Ref != "") {
				t.Fatalf("a non-remote use carries a slug/ref: %+v", u)
			}
		}

		// Whatever Scan produced must audit and render without panicking. A nil
		// resolver keeps this offline; every remote reference then skips.
		rep := Audit(context.Background(), uses, nil)
		if len(rep.Findings) != len(uses) {
			t.Fatalf("Audit returned %d findings for %d uses", len(rep.Findings), len(uses))
		}
		for _, fnd := range rep.Findings {
			switch fnd.Status {
			case StatusPinned, StatusStale, StatusUnpinned, StatusSkipped:
			default:
				t.Fatalf("Audit emitted an unknown status %d: %+v", fnd.Status, fnd)
			}
		}
		Render(io.Discard, rep)
		if err := EncodeJSON(io.Discard, rep); err != nil {
			t.Fatalf("EncodeJSON failed: %v", err)
		}
	})
}
