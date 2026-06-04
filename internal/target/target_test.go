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

func TestResolveRepo(t *testing.T) {
	cases := []struct {
		in          []string
		owner, repo string
		wantErr     bool
	}{
		{[]string{"justanotherspy/shuck"}, "justanotherspy", "shuck", false},
		{[]string{"https://github.com/justanotherspy/shuck"}, "justanotherspy", "shuck", false},
		{[]string{"https://github.com/justanotherspy/shuck.git"}, "justanotherspy", "shuck", false},
		{[]string{"github.com/justanotherspy/shuck"}, "justanotherspy", "shuck", false},
		{[]string{"https://github.com/justanotherspy/shuck/pull/42"}, "justanotherspy", "shuck", false},
		{[]string{"https://github.com/justanotherspy/shuck/actions/runs/123"}, "justanotherspy", "shuck", false},
		{[]string{"https://github.com/justanotherspy/shuck/tree/main"}, "justanotherspy", "shuck", false},
		{[]string{"not-a-repo"}, "", "", true},
		{[]string{"a", "b"}, "", "", true},
	}
	for _, c := range cases {
		owner, repo, err := ResolveRepo(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ResolveRepo(%v): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveRepo(%v): %v", c.in, err)
			continue
		}
		if owner != c.owner || repo != c.repo {
			t.Errorf("ResolveRepo(%v) = %q/%q, want %q/%q", c.in, owner, repo, c.owner, c.repo)
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

func TestParseActionsURL(t *testing.T) {
	cases := []struct {
		in           string
		owner, repo  string
		runID, jobID int64
		attempt      int
		wantOK       bool
	}{
		{"https://github.com/justanotherspy/shuck/actions/runs/123", "justanotherspy", "shuck", 123, 0, 0, true},
		{"http://github.com/justanotherspy/shuck/actions/runs/123", "justanotherspy", "shuck", 123, 0, 0, true},
		{"github.com/justanotherspy/shuck/actions/runs/123", "justanotherspy", "shuck", 123, 0, 0, true},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/job/456", "justanotherspy", "shuck", 123, 456, 0, true},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/jobs/456", "justanotherspy", "shuck", 123, 456, 0, true},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/job/456?pr=42", "justanotherspy", "shuck", 123, 456, 0, true},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/attempts/2", "justanotherspy", "shuck", 123, 0, 2, true},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/attempt/2", "justanotherspy", "shuck", 123, 0, 2, true},
		{"https://github.example.com/acme/widgets/actions/runs/7/job/8", "acme", "widgets", 7, 8, 0, true},
		// Not Actions URLs: fall through to other resolution.
		{"https://github.com/justanotherspy/shuck/pull/12", "", "", 0, 0, 0, false},
		{"https://github.com/justanotherspy/shuck/actions/runs/notanumber", "", "", 0, 0, 0, false},
		{"https://github.com/justanotherspy/shuck/actions/runs/0", "", "", 0, 0, 0, false},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/job/0", "", "", 0, 0, 0, false},
		{"https://github.com/justanotherspy/shuck/actions/runs/123/attempts/0", "", "", 0, 0, 0, false},
		{"https://github.com/justanotherspy/shuck/actions", "", "", 0, 0, 0, false},
		{"42", "", "", 0, 0, 0, false},
	}
	for _, c := range cases {
		ref, ok := parseActionsURL(c.in)
		if ok != c.wantOK {
			t.Errorf("parseActionsURL(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if ref.Owner != c.owner || ref.Repo != c.repo || ref.RunID != c.runID || ref.JobID != c.jobID || ref.Attempt != c.attempt {
			t.Errorf("parseActionsURL(%q) = %q/%q run=%d job=%d attempt=%d, want %q/%q run=%d job=%d attempt=%d",
				c.in, ref.Owner, ref.Repo, ref.RunID, ref.JobID, ref.Attempt, c.owner, c.repo, c.runID, c.jobID, c.attempt)
		}
	}

	run, err := Resolve([]string{"https://github.com/justanotherspy/shuck/actions/runs/55"})
	if err != nil {
		t.Fatalf("Resolve(run URL): unexpected error: %v", err)
	}
	if run.Owner != "justanotherspy" || run.Repo != "shuck" || run.RunID != 55 || run.JobID != 0 || run.Number != 0 {
		t.Errorf("Resolve(run URL) = %+v", run)
	}

	job, err := Resolve([]string{"https://github.com/justanotherspy/shuck/actions/runs/55/job/66"})
	if err != nil {
		t.Fatalf("Resolve(job URL): unexpected error: %v", err)
	}
	if job.RunID != 55 || job.JobID != 66 {
		t.Errorf("Resolve(job URL) = %+v", job)
	}

	att, err := Resolve([]string{"https://github.com/justanotherspy/shuck/actions/runs/55/attempts/3"})
	if err != nil {
		t.Fatalf("Resolve(attempt URL): unexpected error: %v", err)
	}
	if att.RunID != 55 || att.JobID != 0 || att.Attempt != 3 {
		t.Errorf("Resolve(attempt URL) = %+v", att)
	}
}

func TestParseChecksURL(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		number      int
		checkRunID  int64
		wantOK      bool
	}{
		{"https://github.com/justanotherspy/shuck/pull/42/checks?check_run_id=12345", "justanotherspy", "shuck", 42, 12345, true},
		{"https://github.com/justanotherspy/shuck/pull/42/checks?check_run_id=12345&foo=bar", "justanotherspy", "shuck", 42, 12345, true},
		{"github.com/justanotherspy/shuck/pull/42?check_run_id=7", "justanotherspy", "shuck", 42, 7, true},
		// No check_run_id: fall through to the plain PR parser.
		{"https://github.com/justanotherspy/shuck/pull/42/checks", "", "", 0, 0, false},
		{"https://github.com/justanotherspy/shuck/pull/42", "", "", 0, 0, false},
		// check_run_id present but not a PR URL.
		{"https://github.com/justanotherspy/shuck/actions/runs/1?check_run_id=7", "", "", 0, 0, false},
		// Non-positive / non-numeric check_run_id.
		{"https://github.com/justanotherspy/shuck/pull/42/checks?check_run_id=0", "", "", 0, 0, false},
		{"https://github.com/justanotherspy/shuck/pull/42/checks?check_run_id=abc", "", "", 0, 0, false},
	}
	for _, c := range cases {
		owner, repo, number, id, ok := parseChecksURL(c.in)
		if ok != c.wantOK {
			t.Errorf("parseChecksURL(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if owner != c.owner || repo != c.repo || number != c.number || id != c.checkRunID {
			t.Errorf("parseChecksURL(%q) = %q/%q#%d check=%d, want %q/%q#%d check=%d",
				c.in, owner, repo, number, id, c.owner, c.repo, c.number, c.checkRunID)
		}
	}

	tgt, err := Resolve([]string{"https://github.com/justanotherspy/shuck/pull/42/checks?check_run_id=99"})
	if err != nil {
		t.Fatalf("Resolve(checks URL): unexpected error: %v", err)
	}
	if tgt.Owner != "justanotherspy" || tgt.Repo != "shuck" || tgt.Number != 42 || tgt.CheckRunID != 99 || tgt.RunID != 0 {
		t.Errorf("Resolve(checks URL) = %+v", tgt)
	}
}
