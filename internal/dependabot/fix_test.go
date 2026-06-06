package dependabot

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// bareEntry is one update entry missing every best-practice field shuck fills.
const bareEntry = `version: 2
updates:
  # keep me
  - package-ecosystem: github-actions
    directory: /
    schedule:
      interval: weekly
`

func TestFixAddsMissingFields(t *testing.T) {
	res, err := Fix([]byte(bareEntry))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected the config to change")
	}
	if len(res.Fixed) != 1 {
		t.Fatalf("expected 1 fixed entry, got %+v", res.Fixed)
	}
	fe := res.Fixed[0]
	if fe.Ecosystem != "github-actions" || fe.Directory != "/" {
		t.Errorf("entry id wrong: %+v", fe)
	}
	for _, want := range []string{"groups", "labels", "cooldown", "open-pull-requests-limit", "commit-message"} {
		if !contains(fe.Added, want) {
			t.Errorf("expected %q in added fields, got %v", want, fe.Added)
		}
	}

	body := string(res.Data)
	if !strings.Contains(body, "# keep me") {
		t.Errorf("existing comment lost:\n%s", body)
	}
	// github-actions uses the ci commit prefix and a 7-day cooldown.
	for _, want := range []string{"default-days: 7", "open-pull-requests-limit: 10", "prefix: ci", "github-actions-minor-and-patch"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// The result must round-trip through the strict parser.
	if _, err := Parse(res.Data); err != nil {
		t.Fatalf("fixed config does not parse: %v\n%s", err, body)
	}
	// No assignees in the entry → a note nudges the user to add them.
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "assignees") {
		t.Errorf("expected an assignees note, got %v", res.Notes)
	}
}

func TestFixPreservesExistingFields(t *testing.T) {
	// An entry already setting cooldown/limit/commit-message keeps its values;
	// only the genuinely missing groups + labels are added.
	existing := `version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
    assignees: [alice]
    cooldown:
      default-days: 3
    open-pull-requests-limit: 2
    commit-message:
      prefix: deps
`
	res, err := Fix([]byte(existing))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Changed {
		t.Fatal("groups and labels were missing — expected a change")
	}
	added := res.Fixed[0].Added
	if contains(added, "cooldown") || contains(added, "open-pull-requests-limit") || contains(added, "commit-message") {
		t.Errorf("must not re-add present fields, got %v", added)
	}
	if !contains(added, "groups") || !contains(added, "labels") {
		t.Errorf("expected groups+labels added, got %v", added)
	}
	body := string(res.Data)
	// Existing values are untouched.
	for _, want := range []string{"default-days: 3", "open-pull-requests-limit: 2", "prefix: deps"} {
		if !strings.Contains(body, want) {
			t.Errorf("existing value changed; missing %q in:\n%s", want, body)
		}
	}
	// An entry with assignees yields no assignees note.
	if len(res.Notes) != 0 {
		t.Errorf("did not expect notes for an assigned entry, got %v", res.Notes)
	}
}

func TestFixUpToDate(t *testing.T) {
	// An entry already complete (and assigned) is a no-op.
	complete := `version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
    assignees: [alice]
    labels: [dependencies]
    cooldown:
      default-days: 7
    open-pull-requests-limit: 10
    commit-message:
      prefix: chore
    groups:
      all:
        patterns: ["*"]
`
	res, err := Fix([]byte(complete))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Changed {
		t.Errorf("nothing should change:\n%s", res.Data)
	}
	if len(res.Notes) == 0 {
		t.Error("expected an up-to-date note")
	}
	if !bytes.Equal(res.Data, []byte(complete)) {
		t.Error("up-to-date fix must return the input unchanged")
	}
}

func TestFixIdempotent(t *testing.T) {
	first, err := Fix([]byte(bareEntry))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	second, err := Fix(first.Data)
	if err != nil {
		t.Fatalf("Fix (second pass): %v", err)
	}
	if second.Changed {
		t.Errorf("second fix should be a no-op:\n%s", second.Data)
	}
}

// contains reports whether v is in s.
func contains(s []string, v string) bool { return slices.Contains(s, v) }

func TestFixInvalidConfig(t *testing.T) {
	if _, err := Fix([]byte("version: 1\nupdates: []\n")); err == nil {
		t.Fatal("expected a parse error for an invalid config")
	}
}

func TestFixDirectoriesEntry(t *testing.T) {
	// A multi-directory entry reports the first directory in its summary.
	existing := `version: 2
updates:
  - package-ecosystem: npm
    directories:
      - /a
      - /b
    schedule:
      interval: weekly
`
	res, err := Fix([]byte(existing))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed[0].Directory != "/a" {
		t.Errorf("directory = %q, want /a", res.Fixed[0].Directory)
	}
}

func TestRenderFix(t *testing.T) {
	tests := []struct {
		name   string
		res    FixResult
		dryRun bool
		want   []string
	}{
		{
			"updated",
			FixResult{Owner: "o", Repo: "r", Path: "p", Changed: true, Data: []byte("version: 2\n"),
				Fixed: []FixedEntry{{Ecosystem: "gomod", Directory: "/", Added: []string{"cooldown"}}}},
			false,
			[]string{"Updated 1 update entry(ies)", "gomod (/): +cooldown", "version: 2"},
		},
		{
			"would update",
			FixResult{Owner: "o", Repo: "r", Path: "p", Changed: true, Data: []byte("x"),
				Fixed: []FixedEntry{{Ecosystem: "npm", Added: []string{"labels"}}}},
			true,
			[]string{"Would update", "npm: +labels"},
		},
		{
			"uptodate",
			FixResult{Owner: "o", Repo: "r", Path: "p", Notes: []string{"all set"}},
			false,
			[]string{"every entry already sets", "all set"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			RenderFix(&b, &tt.res, tt.dryRun)
			for _, w := range tt.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("missing %q in:\n%s", w, b.String())
				}
			}
		})
	}
}

func TestEncodeFixJSON(t *testing.T) {
	r := FixResult{
		Owner: "o", Repo: "r", Path: "p", Changed: true, Data: []byte("version: 2\n"),
		Fixed: []FixedEntry{{Ecosystem: "gomod", Directory: "/", Added: []string{"cooldown"}}},
		Notes: []string{"add assignees"},
	}
	var b bytes.Buffer
	if err := EncodeFixJSON(&b, &r); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc FixDocument
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b.String())
	}
	if !doc.Changed || doc.UpToDate {
		t.Errorf("flags wrong: %+v", doc)
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("schema = %d", doc.SchemaVersion)
	}
	if len(doc.Fixed) != 1 || doc.Fixed[0].Ecosystem != "gomod" || doc.Fixed[0].Added[0] != "cooldown" {
		t.Errorf("fixed = %+v", doc.Fixed)
	}
	if doc.Config != "version: 2\n" {
		t.Errorf("config = %q", doc.Config)
	}
}

func TestNewFixDocumentNonNil(t *testing.T) {
	doc := NewFixDocument(&FixResult{Owner: "o", Repo: "r"})
	if doc.Fixed == nil || doc.Notes == nil {
		t.Error("slices must be non-nil")
	}
	if !doc.UpToDate {
		t.Error("an unchanged fix should be up to date")
	}
}
