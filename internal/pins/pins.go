// Package pins finds the GitHub Actions `uses:` references in a repository's
// workflow files and reports which of them are not pinned to an immutable
// commit SHA — the supply-chain control a mutable tag like `@v4` silently
// gives up, because whoever can move that tag can change what runs in CI.
//
// The package splits cleanly in two so both halves stay testable offline.
// Scan is pure text work: it parses workflow YAML into the `uses:` references
// it declares, with the file and line they live on. Audit classifies those
// references against the latest release of each action, asking a caller-
// supplied Resolver for the network part. Nothing here dials out, so the CLI,
// the MCP server and the file-watching monitor can all drive the same engine.
//
// Scanning never fails as a whole: a workflow whose YAML does not parse is
// reported as one skipped finding, so a single broken file cannot hide the
// rest of the repository's pins.
package pins

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// UseKind classifies where a `uses:` reference points, because only a remote
// action (owner/repo on GitHub) can be pinned to a commit SHA at all.
type UseKind int

// The kinds of `uses:` reference shuck distinguishes.
const (
	UseRemote  UseKind = iota // owner/repo[/subpath][@ref] — pinnable
	UseLocal                  // ./path or . — lives in this repo, nothing to pin
	UseDocker                 // docker://image — pin the image digest instead
	UseInvalid                // a placeholder for a file whose YAML did not parse
)

// String returns the lowercase name of the kind, used in the JSON view.
func (k UseKind) String() string {
	switch k {
	case UseRemote:
		return "remote"
	case UseLocal:
		return "local"
	case UseDocker:
		return "docker"
	case UseInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// Use is one `uses:` reference found in a workflow file, kept together with
// exactly enough source location to point a human (or an agent) at the line
// that has to change.
//
// A Use with Kind UseInvalid is not a real reference: it stands in for a file
// whose YAML could not be parsed, carrying the reason in Err so the audit can
// surface it as a skipped finding rather than swallowing the file.
type Use struct {
	File    string  // repo-relative path, e.g. ".github/workflows/ci.yml"
	Line    int     // 1-based line number of the `uses:` key
	Raw     string  // the reference exactly as written, e.g. "actions/checkout@v4"
	Slug    string  // "actions/checkout" (owner/repo[/subpath]); "" for local/docker refs
	Ref     string  // the part after "@": a SHA, tag, or branch; "" when absent
	Comment string  // the trailing "# v4.2.2" comment's text, without the "#"
	Kind    UseKind // where the reference points
	Err     string  // why this entry is not a usable reference; only set for UseInvalid
}

// maxWalkDepth bounds the recursive YAML walk. Real workflow documents nest a
// handful of levels deep; the cap only exists so a pathological (or fuzzed)
// document cannot drive the walk into a stack overflow.
const maxWalkDepth = 100

// Scan parses the workflow YAML in files (repo-relative path -> content) and
// returns every `uses:` reference it declares, ordered by file then line so the
// output is stable across runs and across map iteration order.
//
// Detection is schema-free on purpose: it walks every document looking for a
// mapping key spelled exactly "uses", which catches `jobs.<id>.steps[].uses`,
// a composite action's `runs.steps[].uses`, and a reusable workflow's
// `jobs.<id>.uses` alike, without shuck having to track GitHub's schema. A
// `uses` appearing as a *value* (`run: uses`) is not a key and never matches.
//
// A file that fails to parse yields a single UseInvalid entry carrying the
// parse error instead of aborting the scan, and any references already read
// from an earlier document in that file are kept.
func Scan(files map[string][]byte) []Use {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []Use
	for _, name := range names {
		out = append(out, scanFile(name, files[name])...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// scanFile decodes every YAML document in one file and collects its `uses:`
// references, appending an UseInvalid marker if a document fails to decode.
func scanFile(name string, data []byte) []Use {
	var out []Use
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			return append(out, Use{
				File: name,
				Line: 1,
				Kind: UseInvalid,
				Err:  fmt.Sprintf("parse %s: %v", name, err),
			})
		}
		collectUses(&doc, name, 0, &out)
	}
}

// collectUses walks a YAML node recursively, appending a Use for every mapping
// entry whose key is "uses" and whose value is a non-empty scalar. Alias nodes
// are not followed: a recursive anchor would otherwise loop forever, and a
// workflow that hides a `uses:` behind an anchor is vanishingly rare next to
// that risk.
func collectUses(n *yaml.Node, file string, depth int, out *[]Use) {
	if n == nil || depth > maxWalkDepth || n.Kind == yaml.AliasNode {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Kind == yaml.ScalarNode && key.Value == "uses" {
				if u, ok := newUse(file, key, val); ok {
					*out = append(*out, u)
				}
			}
		}
	}
	for _, c := range n.Content {
		collectUses(c, file, depth+1, out)
	}
}

// newUse builds a Use from a `uses:` key/value pair. It reports ok=false when
// the value is not a usable scalar (a list, a mapping, or an empty/null value),
// so a malformed entry is ignored rather than turned into a bogus reference.
func newUse(file string, key, val *yaml.Node) (Use, bool) {
	if val == nil || val.Kind != yaml.ScalarNode {
		return Use{}, false
	}
	raw := strings.TrimSpace(val.Value)
	if raw == "" {
		return Use{}, false
	}
	comment := val.LineComment
	if comment == "" {
		comment = key.LineComment
	}
	u := Use{
		File:    file,
		Line:    key.Line,
		Raw:     raw,
		Comment: cleanComment(comment),
		Kind:    classify(raw),
	}
	if u.Line < 1 {
		u.Line = 1
	}
	if u.Kind == UseRemote {
		u.Slug, u.Ref, _ = strings.Cut(raw, "@")
	}
	return u, true
}

// classify decides where a reference points. A leading "." is a path into this
// repository (".", "./composite", "../shared"), "docker://" is a container
// image, and everything else is treated as a remote owner/repo action — the
// only form that can carry a commit SHA.
func classify(raw string) UseKind {
	switch {
	case strings.HasPrefix(raw, "."):
		return UseLocal
	case strings.HasPrefix(raw, "docker://"):
		return UseDocker
	default:
		return UseRemote
	}
}

// cleanComment reduces a yaml.Node line comment ("# v4.2.2") to its text
// ("v4.2.2"). Only the first "#" is stripped, so a comment that itself contains
// a hash keeps it.
func cleanComment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	return strings.TrimSpace(s)
}

// IsSHA reports whether ref is an immutable commit SHA: 40 lowercase hex digits
// (SHA-1) or 64 (SHA-256, for repositories on the newer object format). Git
// prints SHAs lowercase and the pinning convention writes them that way, so an
// uppercase or abbreviated ref is deliberately not accepted — an abbreviated
// SHA is not what GitHub Actions resolves immutably.
func IsSHA(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for i := range len(ref) {
		if c := ref[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maxWorkflowFileSize caps the size of a file WorkflowFiles will read. A
// workflow is a few kilobytes; anything past a megabyte is not one, and reading
// it would only cost memory.
const maxWorkflowFileSize = 1 << 20

// WorkflowFiles walks root and returns the files shuck scans for `uses:`
// references, as repo-relative slash-separated paths mapped to their contents:
// the workflows in .github/workflows, the repository's own action manifest at
// the root, and the action manifests one level deep under .github/actions.
//
// The walk is deliberately narrow rather than a full tree walk — those are the
// only places GitHub itself reads action definitions from, and a bounded walk
// keeps the file-watching monitor cheap. A missing directory is not an error
// (most repositories have no .github/actions), and a file over
// maxWorkflowFileSize is skipped rather than read.
func WorkflowFiles(root string) (map[string][]byte, error) {
	out := map[string][]byte{}

	names, err := dirEntries(root, ".github/workflows")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !isYAML(name) {
			continue
		}
		if err := addFile(out, root, path.Join(".github/workflows", name)); err != nil {
			return nil, err
		}
	}

	for _, name := range actionManifests {
		if err := addFile(out, root, name); err != nil {
			return nil, err
		}
	}

	dirs, err := subdirs(root, ".github/actions")
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		for _, name := range actionManifests {
			if err := addFile(out, root, path.Join(".github/actions", dir, name)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// actionManifests are the two spellings GitHub accepts for an action manifest.
var actionManifests = []string{"action.yml", "action.yaml"}

// dirEntries lists the names in root/rel, treating a missing directory as
// empty. Any other read failure is a real error the caller should see.
func dirEntries(root, rel string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// subdirs lists the subdirectory names in root/rel, treating a missing
// directory as empty.
func subdirs(root, rel string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// addFile reads root/rel into out under its repo-relative slash path. A file
// that does not exist, is a directory, or exceeds the size cap is skipped
// silently; only a genuine read failure is returned.
func addFile(out map[string][]byte, root, rel string) error {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	if info.IsDir() || info.Size() > maxWorkflowFileSize {
		return nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("read %s: %w", rel, err)
	}
	out[rel] = data
	return nil
}

// isYAML reports whether a filename has a YAML extension, in either spelling.
func isYAML(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".yml" || ext == ".yaml"
}
