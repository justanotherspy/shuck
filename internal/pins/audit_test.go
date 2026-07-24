package pins

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/model"
)

// Distinct, obviously-fake commit SHAs so a mix-up in a failure message is
// readable at a glance.
const (
	sha420   = "1111111111111111111111111111111111111111"
	sha422   = "2222222222222222222222222222222222222222"
	sha500   = "3333333333333333333333333333333333333333"
	shaMoved = "4444444444444444444444444444444444444444"
)

// checkoutTags spans two majors so the "newer major exists" rule is exercised.
var checkoutTags = []model.ActionTag{
	{Name: "v4.2.0", SHA: sha420},
	{Name: "v4.2.2", SHA: sha422},
	{Name: "v5.0.0", SHA: sha500},
}

// fakeResolver answers from a fixed tag table using the real action.Select, so
// the audit is exercised against the same selection rules the CLI uses. It
// records every call so de-duplication can be asserted.
type fakeResolver struct {
	tags  map[string][]model.ActionTag
	errs  map[string]error
	calls []string
}

func (f *fakeResolver) Resolve(_ context.Context, ref action.Ref) (action.Resolved, error) {
	f.calls = append(f.calls, ref.Slug()+"@"+ref.Constraint)
	if err := f.errs[ref.Slug()]; err != nil {
		return action.Resolved{}, err
	}
	tag, err := action.Select(f.tags[ref.Slug()], ref.Constraint)
	if err != nil {
		return action.Resolved{}, err
	}
	return action.Resolved{Ref: ref, Tag: tag.Name, SHA: tag.SHA}, nil
}

// scanOne is the shortest path from a workflow snippet to its single use.
func scanOne(t *testing.T, yaml string) Use {
	t.Helper()
	uses := Scan(map[string][]byte{".github/workflows/ci.yml": []byte(yaml)})
	if len(uses) != 1 {
		t.Fatalf("snippet produced %d uses, want 1: %+v", len(uses), uses)
	}
	return uses[0]
}

func TestAudit(t *testing.T) {
	tests := []struct {
		name         string
		yaml         string
		tags         map[string][]model.ActionTag
		errs         map[string]error
		nilResolver  bool
		wantStatus   Status
		wantLatest   string
		wantPin      string
		wantNote     []string
		wantNoteNone []string
	}{
		{
			name:       "mutable major tag",
			yaml:       "steps:\n  - uses: actions/checkout@v4\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusUnpinned,
			wantLatest: "v4.2.2",
			wantPin:    "actions/checkout@" + sha422 + " # v4.2.2",
			wantNote:   []string{`"v4" is a mutable tag`},
		},
		{
			name:       "branch ref resolves to the latest release overall",
			yaml:       "steps:\n  - uses: actions/checkout@main\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusUnpinned,
			wantLatest: "v5.0.0",
			wantPin:    "actions/checkout@" + sha500 + " # v5.0.0",
			wantNote:   []string{`"main" is a branch or non-semver tag`},
		},
		{
			name:       "no version at all",
			yaml:       "steps:\n  - uses: actions/checkout\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusUnpinned,
			wantLatest: "v5.0.0",
			wantPin:    "actions/checkout@" + sha500 + " # v5.0.0",
			wantNote:   []string{"no version at all"},
		},
		{
			name:       "sha pinned and current",
			yaml:       "steps:\n  - uses: actions/setup-go@" + sha500 + " # v5.0.0\n",
			tags:       map[string][]model.ActionTag{"actions/setup-go": {{Name: "v5.0.0", SHA: sha500}}},
			wantStatus: StatusPinned,
			wantLatest: "v5.0.0",
		},
		{
			name:       "sha pinned but behind within the major",
			yaml:       "steps:\n  - uses: actions/checkout@" + sha420 + " # v4.2.0\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusStale,
			wantLatest: "v4.2.2",
			wantPin:    "actions/checkout@" + sha422 + " # v4.2.2",
			wantNote:   []string{"v4.2.0 → v4.2.2", "a newer major v5.0.0 is available"},
		},
		{
			name:       "sha pinned, current major, newer major available",
			yaml:       "steps:\n  - uses: actions/checkout@" + sha422 + " # v4.2.2\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusPinned,
			wantLatest: "v4.2.2",
			// The suggestion must not jump majors, so no pin line is offered.
			wantNote: []string{"a newer major v5.0.0 is available"},
		},
		{
			name: "re-tagged release",
			yaml: "steps:\n  - uses: actions/setup-go@" + shaMoved + " # v5.0.0\n",
			tags: map[string][]model.ActionTag{
				"actions/setup-go": {{Name: "v5.0.0", SHA: sha500}},
			},
			wantStatus: StatusStale,
			wantLatest: "v5.0.0",
			wantPin:    "actions/setup-go@" + sha500 + " # v5.0.0",
			wantNote:   []string{"was re-tagged", sha500},
		},
		{
			name:         "sha pinned without a version comment",
			yaml:         "steps:\n  - uses: actions/checkout@" + sha422 + "\n",
			tags:         map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus:   StatusPinned,
			wantNote:     []string{"no version comment"},
			wantNoteNone: []string{"newer major"},
		},
		{
			name:       "sha pinned with an unparseable version comment",
			yaml:       "steps:\n  - uses: actions/checkout@" + sha422 + " # latest\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusPinned,
			wantNote:   []string{"no version comment"},
		},
		{
			name:       "local reference",
			yaml:       "steps:\n  - uses: ./.github/actions/setup\n",
			wantStatus: StatusSkipped,
			wantNote:   []string{"local action reference"},
		},
		{
			name:       "docker reference",
			yaml:       "steps:\n  - uses: docker://alpine:3.20\n",
			wantStatus: StatusSkipped,
			wantNote:   []string{"docker image reference"},
		},
		{
			name:       "unparseable file",
			yaml:       "jobs:\n  - [unclosed\n",
			wantStatus: StatusSkipped,
			wantNote:   []string{"parse .github/workflows/ci.yml"},
		},
		{
			name:       "reference that is not owner/repo",
			yaml:       "steps:\n  - uses: checkout@v4\n",
			wantStatus: StatusSkipped,
			wantNote:   []string{"cannot read the action reference"},
		},
		{
			// An unpinned ref is unpinned whether or not the latest release
			// could be looked up: a rate-limited run must not under-report.
			name:       "resolver error still reports the ref as unpinned",
			yaml:       "steps:\n  - uses: actions/checkout@v4\n",
			errs:       map[string]error{"actions/checkout": errors.New("403 rate limited")},
			wantStatus: StatusUnpinned,
			wantNote:   []string{"not a commit SHA", "403 rate limited"},
		},
		{
			name:       "resolver error on a sha pin degrades to skipped",
			yaml:       "steps:\n  - uses: actions/checkout@" + sha422 + " # v4.2.2\n",
			errs:       map[string]error{"actions/checkout": errors.New("boom")},
			wantStatus: StatusSkipped,
			wantNote:   []string{"cannot resolve the latest release", "boom"},
		},
		{
			name:        "nil resolver still reports an unpinned reference",
			yaml:        "steps:\n  - uses: actions/checkout@v4\n",
			nilResolver: true,
			wantStatus:  StatusUnpinned,
			wantNote:    []string{"no resolver configured"},
		},
		{
			name:       "no matching release still reports an unpinned reference",
			yaml:       "steps:\n  - uses: actions/checkout@v9\n",
			tags:       map[string][]model.ActionTag{"actions/checkout": checkoutTags},
			wantStatus: StatusUnpinned,
			wantNote:   []string{"no release matches"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Resolver
			if !tt.nilResolver {
				r = &fakeResolver{tags: tt.tags, errs: tt.errs}
			}

			uses := Scan(map[string][]byte{".github/workflows/ci.yml": []byte(tt.yaml)})
			rep := Audit(context.Background(), uses, r)
			if len(rep.Findings) != 1 {
				t.Fatalf("Audit returned %d findings, want 1: %+v", len(rep.Findings), rep.Findings)
			}
			f := rep.Findings[0]

			if f.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v (note %q)", f.Status, tt.wantStatus, f.Note)
			}
			if f.Latest != tt.wantLatest {
				t.Errorf("Latest = %q, want %q", f.Latest, tt.wantLatest)
			}
			if f.PinLine != tt.wantPin {
				t.Errorf("PinLine = %q, want %q", f.PinLine, tt.wantPin)
			}
			for _, want := range tt.wantNote {
				if !strings.Contains(f.Note, want) {
					t.Errorf("Note = %q, want it to contain %q", f.Note, want)
				}
			}
			for _, unwanted := range tt.wantNoteNone {
				if strings.Contains(f.Note, unwanted) {
					t.Errorf("Note = %q, want it not to contain %q", f.Note, unwanted)
				}
			}
			if rep.CheckedAt.IsZero() {
				t.Error("CheckedAt was not set")
			}
		})
	}
}

func TestAuditDedupesResolverCalls(t *testing.T) {
	r := &fakeResolver{tags: map[string][]model.ActionTag{"actions/checkout": checkoutTags}}
	files := map[string][]byte{
		".github/workflows/a.yml": []byte("steps:\n  - uses: actions/checkout@v4\n  - uses: actions/checkout@v4\n"),
		".github/workflows/b.yml": []byte("steps:\n  - uses: actions/checkout@v4\n  - uses: actions/checkout@v5\n"),
	}

	rep := Audit(context.Background(), Scan(files), r)
	if len(rep.Findings) != 4 {
		t.Fatalf("Audit returned %d findings, want 4", len(rep.Findings))
	}
	// Three uses share slug+constraint v4; the fourth asks for v5.
	want := []string{"actions/checkout@v4", "actions/checkout@v5"}
	if len(r.calls) != len(want) {
		t.Fatalf("resolver was called %d times (%v), want %d", len(r.calls), r.calls, len(want))
	}
	for i, w := range want {
		if r.calls[i] != w {
			t.Errorf("call %d = %q, want %q", i, r.calls[i], w)
		}
	}
}

func TestAuditSortsAndTallies(t *testing.T) {
	r := &fakeResolver{tags: map[string][]model.ActionTag{"actions/checkout": checkoutTags}}
	uses := []Use{
		{File: "b.yml", Line: 2, Raw: "./local", Kind: UseLocal},
		{File: "a.yml", Line: 9, Raw: "actions/checkout@v4", Slug: "actions/checkout", Ref: "v4", Kind: UseRemote},
		{File: "a.yml", Line: 2, Raw: "actions/checkout@" + sha420, Slug: "actions/checkout", Ref: sha420, Comment: "v4.2.0", Kind: UseRemote},
		{File: "a.yml", Line: 5, Raw: "actions/checkout@" + sha422, Slug: "actions/checkout", Ref: sha422, Comment: "v4.2.2", Kind: UseRemote},
	}

	rep := Audit(context.Background(), uses, r)

	var order []string
	for _, f := range rep.Findings {
		order = append(order, f.File)
	}
	wantOrder := []string{"a.yml", "a.yml", "a.yml", "b.yml"}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("finding order = %v, want %v", order, wantOrder)
		}
	}
	if rep.Findings[0].Line != 2 || rep.Findings[1].Line != 5 || rep.Findings[2].Line != 9 {
		t.Errorf("findings are not ordered by line: %+v", rep.Findings)
	}

	if rep.Unpinned != 1 || rep.Stale != 1 || rep.Skipped != 1 {
		t.Errorf("tally = unpinned %d, stale %d, skipped %d; want 1/1/1", rep.Unpinned, rep.Stale, rep.Skipped)
	}
	if got := rep.Count(StatusPinned); got != 1 {
		t.Errorf("Count(StatusPinned) = %d, want 1", got)
	}
	if !rep.HasIssues() {
		t.Error("HasIssues() = false, want true")
	}
}

func TestReportHasIssues(t *testing.T) {
	tests := []struct {
		name string
		rep  Report
		want bool
	}{
		{"clean", Report{}, false},
		{"skipped only", Report{Skipped: 3}, false},
		{"unpinned", Report{Unpinned: 1}, true},
		{"stale", Report{Stale: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rep.HasIssues(); got != tt.want {
				t.Errorf("HasIssues() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusPinned, "pinned"},
		{StatusStale, "stale"},
		{StatusUnpinned, "unpinned"},
		{StatusSkipped, "skipped"},
		{Status(42), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.status.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepository(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".github/workflows/ci.yml": "steps:\n  - uses: actions/checkout@v4\n",
		"action.yml":               "runs:\n  steps:\n    - uses: actions/checkout@" + sha422 + " # v4.2.2\n",
	})

	r := &fakeResolver{tags: map[string][]model.ActionTag{"actions/checkout": checkoutTags}}
	rep, err := Repository(context.Background(), root, r)
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if rep.Root != root {
		t.Errorf("Root = %q, want %q", rep.Root, root)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("Repository returned %d findings, want 2: %+v", len(rep.Findings), rep.Findings)
	}
	if rep.Unpinned != 1 {
		t.Errorf("Unpinned = %d, want 1", rep.Unpinned)
	}
}

func TestRepositoryPropagatesCollectionErrors(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{".github/workflows": "not a directory"})
	if _, err := Repository(context.Background(), root, nil); err == nil {
		t.Fatal("Repository accepted an unreadable workflows path")
	}
}

// fixedResolver answers every request with the same resolution, so the
// defensive branches that cope with a resolver returning a non-semver tag can
// be reached — action.Select never would, but a future Resolver might.
type fixedResolver struct{ res action.Resolved }

func (f fixedResolver) Resolve(_ context.Context, ref action.Ref) (action.Resolved, error) {
	res := f.res
	res.Ref = ref
	return res, nil
}

func TestAuditNonSemverResolution(t *testing.T) {
	r := fixedResolver{res: action.Resolved{Tag: "latest", SHA: sha500}}
	use := scanOne(t, "steps:\n  - uses: actions/checkout@"+sha422+" # v4.2.2\n")

	rep := Audit(context.Background(), []Use{use}, r)
	f := rep.Findings[0]

	// The tag says nothing comparable, but the SHA moved, so the pin is stale.
	if f.Status != StatusStale {
		t.Fatalf("Status = %v, want stale (note %q)", f.Status, f.Note)
	}
	if !strings.Contains(f.Note, "re-tagged") {
		t.Errorf("Note = %q, want it to mention the re-tag", f.Note)
	}
	// A non-semver "latest" cannot prove a newer major exists.
	if strings.Contains(f.Note, "newer major") {
		t.Errorf("Note = %q, want no newer-major claim", f.Note)
	}
}

func TestCommentVersion(t *testing.T) {
	tests := []struct {
		comment string
		want    string
		ok      bool
	}{
		{"v4.2.2", "v4.2.2", true},
		{"  v4.2.2 pinned by shuck ", "v4.2.2", true},
		{"4.2", "4.2", true},
		{"latest", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.comment, func(t *testing.T) {
			got, ok := commentVersion(tt.comment)
			if ok != tt.ok {
				t.Fatalf("commentVersion(%q) ok = %v, want %v", tt.comment, ok, tt.ok)
			}
			if ok && got.Raw != tt.want {
				t.Errorf("commentVersion(%q) = %q, want %q", tt.comment, got.Raw, tt.want)
			}
		})
	}
}

func TestJoinNotes(t *testing.T) {
	tests := []struct{ note, extra, want string }{
		{"", "b", "b"},
		{"a", "", "a"},
		{"a", "b", "a; b"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := joinNotes(tt.note, tt.extra); got != tt.want {
			t.Errorf("joinNotes(%q, %q) = %q, want %q", tt.note, tt.extra, got, tt.want)
		}
	}
}

// scanOneUsed keeps scanOne referenced from a test so the helper stays honest
// about the snippet shape the other tests rely on.
func TestScanOneHelper(t *testing.T) {
	u := scanOne(t, "steps:\n  - uses: actions/checkout@v4\n")
	if u.Slug != "actions/checkout" {
		t.Fatalf("Slug = %q, want actions/checkout", u.Slug)
	}
}
