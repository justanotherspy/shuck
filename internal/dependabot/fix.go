package dependabot

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// FixResult is the outcome of applying best-practice fields to the existing
// update entries of a Dependabot config. Unlike Discover, Fix never adds or
// removes update entries — it only fills in best-practice fields that an entry
// is missing, preserving the file's comments and key order.
type FixResult struct {
	Owner string
	Repo  string
	Path  string

	Data    []byte       // the resulting file contents
	Changed bool         // Data differs from the input (it was written)
	Fixed   []FixedEntry // per-entry summary of the fields that were added
	Notes   []string     // human-readable notes (e.g. add assignees yourself)
}

// FixedEntry records the best-practice fields added to one update entry.
type FixedEntry struct {
	Ecosystem string
	Directory string
	Added     []string // field keys added, in the order Fix appends them
}

// Fix adds the best-practice fields shuck can safely fill — groups, labels,
// cooldown, open-pull-requests-limit, and a commit-message prefix — to every
// existing update entry that is missing them. Assignees are never added, since
// shuck cannot know who should own the PRs; entries lacking them are noted.
// The input must be a valid v2 config (it is strict-parsed first); the patch is
// applied via yaml.Node so comments and key order survive.
func Fix(existing []byte) (FixResult, error) {
	if _, err := Parse(existing); err != nil {
		return FixResult{}, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return FixResult{}, fmt.Errorf("parse existing config: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return FixResult{}, fmt.Errorf("existing config is not a mapping")
	}
	seq := mappingValue(doc.Content[0], "updates")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return FixResult{}, fmt.Errorf("'updates' in the existing config is not a list")
	}

	var fixed []FixedEntry
	needAssignees := false
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		added, err := fixEntry(item)
		if err != nil {
			return FixResult{}, err
		}
		if len(added) > 0 {
			fixed = append(fixed, FixedEntry{
				Ecosystem: scalarValue(item, "package-ecosystem"),
				Directory: entryDir(item),
				Added:     added,
			})
		}
		if mappingValue(item, "assignees") == nil && mappingValue(item, "reviewers") == nil {
			needAssignees = true
		}
	}

	if len(fixed) == 0 {
		return FixResult{
			Data:  existing,
			Notes: []string{"every update entry already sets the best-practice fields shuck can fill"},
		}, nil
	}

	data, err := marshalNode(&doc)
	if err != nil {
		return FixResult{}, err
	}
	res := FixResult{Data: data, Changed: true, Fixed: fixed}
	if needAssignees {
		res.Notes = append(res.Notes, "add assignees or reviewers — shuck cannot know who should own the PRs")
	}
	return res, nil
}

// fixEntry appends the best-practice keys an update entry is missing and returns
// the keys it added (empty when the entry was already complete). The values
// mirror bestPracticeUpdate, so a fixed entry matches a freshly scaffolded one.
func fixEntry(item *yaml.Node) ([]string, error) {
	eco := scalarValue(item, "package-ecosystem")
	var added []string
	add := func(key string, value any) error {
		var n yaml.Node
		if err := n.Encode(value); err != nil {
			return fmt.Errorf("encode %s: %w", key, err)
		}
		item.Content = append(item.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key}, &n)
		added = append(added, key)
		return nil
	}

	if mappingValue(item, "groups") == nil {
		if err := add("groups", map[string]Group{
			eco + "-minor-and-patch": {Patterns: []string{"*"}, UpdateTypes: []string{"minor", "patch"}},
		}); err != nil {
			return nil, err
		}
	}
	if mappingValue(item, "labels") == nil {
		if err := add("labels", []string{"dependencies"}); err != nil {
			return nil, err
		}
	}
	if mappingValue(item, "cooldown") == nil {
		if err := add("cooldown", Cooldown{DefaultDays: new(defaultCooldownDays)}); err != nil {
			return nil, err
		}
	}
	if mappingValue(item, "open-pull-requests-limit") == nil {
		if err := add("open-pull-requests-limit", 10); err != nil {
			return nil, err
		}
	}
	if mappingValue(item, "commit-message") == nil {
		if err := add("commit-message", CommitMessage{Prefix: commitPrefix(eco)}); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// scalarValue returns the string value of a scalar mapping entry, or "".
func scalarValue(m *yaml.Node, key string) string {
	if v := mappingValue(m, key); v != nil {
		return v.Value
	}
	return ""
}

// entryDir returns the entry's directory for display: its `directory`, or the
// first of its `directories`, or "".
func entryDir(m *yaml.Node) string {
	if v := mappingValue(m, "directory"); v != nil {
		return v.Value
	}
	if v := mappingValue(m, "directories"); v != nil && v.Kind == yaml.SequenceNode && len(v.Content) > 0 {
		return v.Content[0].Value
	}
	return ""
}

// RenderFix writes the human-readable summary of a fix run to w. dryRun switches
// the verb to the conditional and prints the would-be file.
func RenderFix(w io.Writer, r *FixResult, dryRun bool) {
	fmt.Fprintf(w, "%s/%s — dependabot fix\n", r.Owner, r.Repo)
	if r.Path != "" {
		fmt.Fprintf(w, "config: %s\n", r.Path)
	}

	if len(r.Fixed) == 0 {
		fmt.Fprintf(w, "\n✓ %s — every entry already sets the best-practice fields shuck can fill.\n", r.Path)
	} else {
		verb := "Updated"
		if dryRun {
			verb = "Would update"
		}
		fmt.Fprintf(w, "\n%s %d update entry(ies) in %s:\n", verb, len(r.Fixed), r.Path)
		for _, e := range r.Fixed {
			loc := e.Ecosystem
			if e.Directory != "" {
				loc += " (" + e.Directory + ")"
			}
			fmt.Fprintf(w, "  – %s: +%s\n", loc, strings.Join(e.Added, ", "))
		}
	}

	for _, n := range r.Notes {
		fmt.Fprintf(w, "  – %s\n", n)
	}

	if r.Changed && len(r.Fixed) > 0 {
		fmt.Fprintf(w, "\n%s", r.Data)
	}
}

// FixDocument is the stable, machine-readable shape of
// `shuck dependabot fix --json`.
type FixDocument struct {
	SchemaVersion int             `json:"schema_version"`
	Repo          Repo            `json:"repo"`
	Path          string          `json:"path"`
	Changed       bool            `json:"changed"`
	UpToDate      bool            `json:"up_to_date"`
	Fixed         []FixedEntryDoc `json:"fixed"`
	Notes         []string        `json:"notes"`
	Config        string          `json:"config"`
}

// FixedEntryDoc is one entry's fix summary in the JSON document.
type FixedEntryDoc struct {
	Ecosystem string   `json:"ecosystem"`
	Directory string   `json:"directory"`
	Added     []string `json:"added"`
}

// NewFixDocument projects a fix result into the stable JSON document. The slices
// are always non-nil so they serialize as [] rather than null.
func NewFixDocument(r *FixResult) FixDocument {
	fixed := make([]FixedEntryDoc, 0, len(r.Fixed))
	for _, e := range r.Fixed {
		added := e.Added
		if added == nil {
			added = []string{}
		}
		fixed = append(fixed, FixedEntryDoc{Ecosystem: e.Ecosystem, Directory: e.Directory, Added: added})
	}
	notes := r.Notes
	if notes == nil {
		notes = []string{}
	}
	return FixDocument{
		SchemaVersion: SchemaVersion,
		Repo:          Repo{Owner: r.Owner, Repo: r.Repo},
		Path:          r.Path,
		Changed:       r.Changed,
		UpToDate:      !r.Changed,
		Fixed:         fixed,
		Notes:         notes,
		Config:        string(r.Data),
	}
}

// EncodeFixJSON writes the fix result as an indented JSON document with a
// trailing newline.
func EncodeFixJSON(w io.Writer, r *FixResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewFixDocument(r))
}
