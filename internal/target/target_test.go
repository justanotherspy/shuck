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
