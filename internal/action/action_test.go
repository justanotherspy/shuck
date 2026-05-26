package action

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in                               string
		owner, repo, subpath, constraint string
		wantErr                          bool
	}{
		{in: "actions/checkout", owner: "actions", repo: "checkout"},
		{in: "actions/checkout@v3", owner: "actions", repo: "checkout", constraint: "v3"},
		{in: " actions/setup-node@3.1 ", owner: "actions", repo: "setup-node", constraint: "3.1"},
		{in: "github/codeql-action/init@v2", owner: "github", repo: "codeql-action", subpath: "init", constraint: "v2"},
		{in: "github/codeql-action/tools/x", owner: "github", repo: "codeql-action", subpath: "tools/x"},
		{in: "not-a-slug", wantErr: true},
		{in: "owner/", wantErr: true},
		{in: "/repo", wantErr: true},
		{in: "owner/repo@", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRef(%q) = %+v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q) error: %v", tc.in, err)
			continue
		}
		if got.Owner != tc.owner || got.Repo != tc.repo || got.Subpath != tc.subpath || got.Constraint != tc.constraint {
			t.Errorf("ParseRef(%q) = %+v, want owner=%q repo=%q subpath=%q constraint=%q",
				tc.in, got, tc.owner, tc.repo, tc.subpath, tc.constraint)
		}
	}
}

func TestRefSlugs(t *testing.T) {
	plain := Ref{Owner: "actions", Repo: "checkout"}
	if got := plain.Slug(); got != "actions/checkout" {
		t.Errorf("Slug() = %q", got)
	}
	sub := Ref{Owner: "github", Repo: "codeql-action", Subpath: "init"}
	if got := sub.Slug(); got != "github/codeql-action/init" {
		t.Errorf("Slug() with subpath = %q", got)
	}
	if got := sub.RepoSlug(); got != "github/codeql-action" {
		t.Errorf("RepoSlug() = %q", got)
	}
}

// tags is a representative tag list: floating major tags, several full releases,
// a prerelease, and a non-semver tag that must be ignored.
func tagsFixture() []model.ActionTag {
	return []model.ActionTag{
		{Name: "v1", SHA: "sha-v1"},
		{Name: "v2", SHA: "sha-v2float"},
		{Name: "v2.1.0", SHA: "sha-210"},
		{Name: "v2.2.0", SHA: "sha-220"},
		{Name: "v2.2.1", SHA: "sha-221"},
		{Name: "v3.0.0", SHA: "sha-300"},
		{Name: "v3.0.0-rc.1", SHA: "sha-300rc1"},
		{Name: "latest", SHA: "sha-latest"}, // non-semver, ignored
		{Name: "v10.0.0", SHA: "sha-1000"},  // exercises numeric (not lexical) ordering
	}
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		wantTag    string
		wantErr    bool
	}{
		{name: "latest overall picks highest numeric stable", constraint: "", wantTag: "v10.0.0"},
		{name: "major v2 picks highest v2.x.x over float", constraint: "v2", wantTag: "v2.2.1"},
		{name: "major without v prefix", constraint: "2", wantTag: "v2.2.1"},
		{name: "major.minor", constraint: "v2.2", wantTag: "v2.2.1"},
		{name: "major.minor with no patch beyond .0", constraint: "2.1", wantTag: "v2.1.0"},
		{name: "exact", constraint: "v2.2.0", wantTag: "v2.2.0"},
		{name: "stable beats prerelease for v3", constraint: "v3", wantTag: "v3.0.0"},
		{name: "no match", constraint: "v9", wantErr: true},
		{name: "bad constraint", constraint: "v3.x", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(tagsFixture(), tc.constraint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Select(%q) = %+v, want error", tc.constraint, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select(%q) error: %v", tc.constraint, err)
			}
			if got.Name != tc.wantTag {
				t.Errorf("Select(%q) tag = %q, want %q", tc.constraint, got.Name, tc.wantTag)
			}
		})
	}
}

func TestSelectPrereleaseFallback(t *testing.T) {
	// When a constraint matches only prereleases, the newest prerelease is
	// returned rather than erroring out.
	tags := []model.ActionTag{
		{Name: "v5.0.0-rc.1", SHA: "rc1"},
		{Name: "v5.0.0-rc.2", SHA: "rc2"},
	}
	got, err := Select(tags, "v5")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name != "v5.0.0-rc.2" {
		t.Errorf("prerelease fallback picked %q, want v5.0.0-rc.2", got.Name)
	}
}

func TestSelectEmptyAndNonSemver(t *testing.T) {
	if _, err := Select(nil, ""); err == nil {
		t.Error("Select(nil) should error")
	}
	if _, err := Select([]model.ActionTag{{Name: "nightly"}, {Name: "latest"}}, ""); err == nil {
		t.Error("Select with only non-semver tags should error")
	}
}

func TestResolvedFormatting(t *testing.T) {
	r := Resolved{
		Ref: Ref{Owner: "actions", Repo: "checkout", Constraint: "v4"},
		Tag: "v4.2.2",
		SHA: "deadbeef",
	}
	if got := r.UsesRef(); got != "actions/checkout@deadbeef" {
		t.Errorf("UsesRef() = %q", got)
	}
	if got := r.PinLine(); got != "actions/checkout@deadbeef # v4.2.2" {
		t.Errorf("PinLine() = %q", got)
	}

	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	for _, want := range []string{"actions/checkout\n", "tag: v4.2.2", "sha: deadbeef", "pin: actions/checkout@deadbeef # v4.2.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render output missing %q:\n%s", want, out)
		}
	}
}

func TestNewDocument(t *testing.T) {
	r := Resolved{
		Ref: Ref{Owner: "github", Repo: "codeql-action", Subpath: "init", Constraint: "v3"},
		Tag: "v3.1.0",
		SHA: "cafe",
	}
	doc := NewDocument(r)
	want := Document{
		SchemaVersion: SchemaVersion,
		Action:        "github/codeql-action/init",
		Owner:         "github",
		Repo:          "codeql-action",
		Subpath:       "init",
		Requested:     "v3",
		Tag:           "v3.1.0",
		SHA:           "cafe",
		Ref:           "github/codeql-action/init@cafe",
		Pin:           "github/codeql-action/init@cafe # v3.1.0",
	}
	if doc != want {
		t.Errorf("NewDocument() = %+v, want %+v", doc, want)
	}
}

func TestEncodeJSON(t *testing.T) {
	r := Resolved{
		Ref: Ref{Owner: "github", Repo: "codeql-action", Subpath: "init", Constraint: "v3"},
		Tag: "v3.1.0",
		SHA: "cafe",
	}
	var b bytes.Buffer
	if err := EncodeJSON(&b, r); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		`"schema_version": 1`,
		`"action": "github/codeql-action/init"`,
		`"subpath": "init"`,
		`"requested": "v3"`,
		`"tag": "v3.1.0"`,
		`"sha": "cafe"`,
		`"ref": "github/codeql-action/init@cafe"`,
		`"pin": "github/codeql-action/init@cafe # v3.1.0"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON missing %q:\n%s", want, out)
		}
	}
}
