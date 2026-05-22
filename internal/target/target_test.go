package target

import "testing"

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		wantErr     bool
	}{
		{"git@github.com:justanotherspy/shuck.git", "justanotherspy", "shuck", false},
		{"git@github.com:justanotherspy/shuck", "justanotherspy", "shuck", false},
		{"https://github.com/justanotherspy/shuck.git", "justanotherspy", "shuck", false},
		{"https://github.com/justanotherspy/shuck", "justanotherspy", "shuck", false},
		{"ssh://git@github.com/justanotherspy/shuck.git", "justanotherspy", "shuck", false},
		{"ssh://git@github.com:22/justanotherspy/shuck.git", "justanotherspy", "shuck", false},
		{"https://github.com/justanotherspy/shuck/", "justanotherspy", "shuck", false},
		{"not-a-remote", "", "", true},
	}
	for _, c := range cases {
		owner, repo, err := ParseRemote(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRemote(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRemote(%q): %v", c.in, err)
			continue
		}
		if owner != c.owner || repo != c.repo {
			t.Errorf("ParseRemote(%q) = %q/%q, want %q/%q", c.in, owner, repo, c.owner, c.repo)
		}
	}
}

func TestResolveArgs(t *testing.T) {
	tgt, err := Resolve([]string{"justanotherspy/shuck", "42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.Owner != "justanotherspy" || tgt.Repo != "shuck" || tgt.Number != 42 {
		t.Errorf("got %+v", tgt)
	}

	if _, err := Resolve([]string{"justanotherspy/shuck", "notanumber"}); err == nil {
		t.Errorf("expected error for non-numeric PR")
	}
	if _, err := Resolve([]string{"noslash"}); err == nil {
		t.Errorf("expected error for single non-numeric arg")
	}
	if _, err := Resolve([]string{"a", "b", "c"}); err == nil {
		t.Errorf("expected error for too many args")
	}
}

func TestResolvePRURL(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		number      int
		wantOK      bool
	}{
		{"https://github.com/justanotherspy/shuck/pull/12", "justanotherspy", "shuck", 12, true},
		{"http://github.com/justanotherspy/shuck/pull/12", "justanotherspy", "shuck", 12, true},
		{"github.com/justanotherspy/shuck/pull/12", "justanotherspy", "shuck", 12, true},
		{"https://github.com/justanotherspy/shuck/pull/12/files", "justanotherspy", "shuck", 12, true},
		{"https://github.com/justanotherspy/shuck/pull/12?w=1#discussion", "justanotherspy", "shuck", 12, true},
		{"https://github.example.com/acme/widgets/pull/7", "acme", "widgets", 7, true},
		// Not PR URLs: fall through to other resolution.
		{"https://github.com/justanotherspy/shuck/issues/12", "", "", 0, false},
		{"https://github.com/justanotherspy/shuck/pull/notanumber", "", "", 0, false},
		{"https://github.com/justanotherspy/shuck/pull/0", "", "", 0, false},
		{"42", "", "", 0, false},
	}
	for _, c := range cases {
		owner, repo, n, ok := parsePRURL(c.in)
		if ok != c.wantOK {
			t.Errorf("parsePRURL(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if owner != c.owner || repo != c.repo || n != c.number {
			t.Errorf("parsePRURL(%q) = %q/%q#%d, want %q/%q#%d", c.in, owner, repo, n, c.owner, c.repo, c.number)
		}
	}

	tgt, err := Resolve([]string{"https://github.com/justanotherspy/shuck/pull/12"})
	if err != nil {
		t.Fatalf("Resolve(PR URL): unexpected error: %v", err)
	}
	if tgt.Owner != "justanotherspy" || tgt.Repo != "shuck" || tgt.Number != 12 {
		t.Errorf("Resolve(PR URL) = %+v", tgt)
	}
}
