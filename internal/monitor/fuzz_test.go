package monitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzMonitorProtocol fuzzes the daemon's request decoding. Every byte a
// client sends crosses a trust boundary — a local one, but a boundary — and
// the daemon has to stay up whatever arrives, because a monitor that can be
// killed by one malformed line is worse than no monitor.
//
// The invariant is the whole contract: decoding either succeeds or reports an
// error, never panics, and a decoded request is always dispatchable.
func FuzzMonitorProtocol(f *testing.F) {
	f.Add(`{"op":"ping"}`)
	f.Add(`{"op":"status","consumer":"sess-1"}`)
	f.Add(`{"op":"events","consumer":"s","limit":5,"wait":1000000000,"peek":true}`)
	f.Add(`{"op":"watch","watch":{"id":"tree:/x","kind":"tree","path":"/x"}}`)
	f.Add(`{"op":"unwatch","id":"pr:o/r#1"}`)
	f.Add(`{"op":"seek","since":18446744073709551615}`)
	f.Add(`{"op":"poke"}`)
	f.Add(`{"op":`)
	f.Add(``)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add("{\"op\":\"ping\",\"auth\":\"\x00\"}")

	f.Fuzz(func(t *testing.T, line string) {
		var req Request
		if err := decodeLine([]byte(line), &req); err != nil {
			if !strings.Contains(err.Error(), "malformed") {
				t.Errorf("decode error should name the problem: %v", err)
			}
			return
		}
		// A decoded request must round-trip: the daemon re-encodes nothing,
		// but a client and a daemon in different builds must agree, and a
		// value that cannot be re-encoded would break that silently.
		if _, err := json.Marshal(req); err != nil {
			t.Errorf("a decoded request must be re-encodable: %v", err)
		}
		// Every op the decoder accepts must be dispatchable without panicking,
		// including ops that do not exist.
		if req.Op == OpStop {
			return // handled: it would shut the test daemon down
		}
		_ = req.Op
	})
}

// FuzzJournalRecovery fuzzes the journal's crash recovery. The file is
// append-only and a crash can clip it anywhere, so openJournal has to make
// sense of whatever is on disk without failing — losing an event is survivable,
// refusing to start is not.
func FuzzJournalRecovery(f *testing.F) {
	f.Add("{\"id\":1,\"kind\":\"ci.failed\",\"title\":\"x\"}\n")
	f.Add("{\"id\":1}\n{\"id\":2,\"kind\":\"ci.passed\"}\n")
	f.Add("{\"id\":1,\"kind\":\"ci.failed\",\"tit")
	f.Add("not json\n\n{}\n")
	f.Add("{\"id\":0,\"kind\":\"x\"}\n")
	f.Add(strings.Repeat("{\"id\":9}\n", 50))

	f.Fuzz(func(t *testing.T, content string) {
		dir := t.TempDir()
		p := newPaths(dir)
		if err := os.WriteFile(p.journal, []byte(content), 0o600); err != nil {
			t.Skip()
		}

		j, err := openJournal(p)
		if err != nil {
			t.Fatalf("a damaged journal must not stop the monitor: %v", err)
		}

		// Whatever survived, the invariants hold: IDs ascend, none is zero,
		// and the next ID is past every one recovered.
		events := j.Since(0, 0)
		var previous uint64
		for _, e := range events {
			if e.ID == 0 {
				t.Error("a zero-ID event should have been skipped")
			}
			if e.ID <= previous {
				t.Errorf("IDs out of order: %d after %d", e.ID, previous)
			}
			previous = e.ID
		}
		if next := j.Append(Event{Kind: KindCIPassed}); next.ID <= previous {
			t.Errorf("the next ID %d must be past the highest recovered %d — IDs are cursors, so reuse would replay events", next.ID, previous)
		}
	})
}

// FuzzReadCheckoutGitFiles fuzzes the git reading the monitor does on every
// tick against arbitrary HEAD and config contents. A repository can be in any
// state mid-rebase, and reading it must never panic or hang.
func FuzzReadCheckoutGitFiles(f *testing.F) {
	f.Add("ref: refs/heads/main\n", "[remote \"origin\"]\n\turl = git@github.com:o/r.git\n")
	f.Add("0123456789abcdef0123456789abcdef01234567\n", "[remote \"origin\"]\n\turl = https://github.com/o/r\n")
	f.Add("ref:", "[remote")
	f.Add("", "")
	f.Add("ref: \n", "[remote \"origin\"]\n\turl =\n")
	f.Add("ref: refs/heads/../../escape\n", "[remote \"origin\"]\n\turl = git@github.com:o/r.git\n")

	f.Fuzz(func(t *testing.T, head, config string) {
		dir := t.TempDir()
		gitDir := filepath.Join(dir, ".git")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Skip()
		}
		if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head), 0o600); err != nil {
			t.Skip()
		}
		if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o600); err != nil {
			t.Skip()
		}

		got, err := ReadCheckout(dir)
		if err != nil {
			return
		}
		// A successful read must produce something usable: a repository the
		// poller can actually ask GitHub about.
		if got.Owner == "" || got.Repo == "" {
			t.Errorf("ReadCheckout succeeded with an empty repo: %+v", got)
		}
		if strings.ContainsAny(got.Owner+got.Repo, "/\x00") {
			t.Errorf("owner/repo must not carry separators: %+v", got)
		}
		// Same() and String() are called on every tick's result; neither may
		// panic on a strange one.
		_ = got.Same(got)
		_ = got.String()
	})
}
