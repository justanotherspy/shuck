package pins

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const ciWorkflow = `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683 # v4.2.2
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
      - name: Test
        run: make test
  codeql:
    runs-on: ubuntu-latest
    steps:
      - uses: github/codeql-action/init@v3
`

func TestScan(t *testing.T) {
	tests := []struct {
		name  string
		files map[string][]byte
		want  []Use
	}{
		{
			name:  "realistic ci workflow",
			files: map[string][]byte{".github/workflows/ci.yml": []byte(ciWorkflow)},
			want: []Use{
				{
					File: ".github/workflows/ci.yml", Line: 7,
					Raw:     "actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683",
					Slug:    "actions/checkout",
					Ref:     "11bd71901bbe5b1630ceea73d27597364c9af683",
					Comment: "v4.2.2",
					Kind:    UseRemote,
				},
				{
					File: ".github/workflows/ci.yml", Line: 8,
					Raw: "actions/setup-go@v5", Slug: "actions/setup-go", Ref: "v5", Kind: UseRemote,
				},
				{
					File: ".github/workflows/ci.yml", Line: 16,
					Raw:  "github/codeql-action/init@v3",
					Slug: "github/codeql-action/init", Ref: "v3", Kind: UseRemote,
				},
			},
		},
		{
			name: "composite action manifest",
			files: map[string][]byte{"action.yml": []byte(`name: Composite
runs:
  using: composite
  steps:
    - uses: actions/cache@v4
    - uses: ./nested
`)},
			want: []Use{
				{File: "action.yml", Line: 5, Raw: "actions/cache@v4", Slug: "actions/cache", Ref: "v4", Kind: UseRemote},
				{File: "action.yml", Line: 6, Raw: "./nested", Kind: UseLocal},
			},
		},
		{
			name: "reusable workflow call",
			files: map[string][]byte{".github/workflows/call.yml": []byte(`jobs:
  docker:
    uses: ./.github/workflows/docker.yml
  shared:
    uses: octo/org/.github/workflows/shared.yml@main
`)},
			want: []Use{
				{File: ".github/workflows/call.yml", Line: 3, Raw: "./.github/workflows/docker.yml", Kind: UseLocal},
				{
					File: ".github/workflows/call.yml", Line: 5,
					Raw:  "octo/org/.github/workflows/shared.yml@main",
					Slug: "octo/org/.github/workflows/shared.yml", Ref: "main", Kind: UseRemote,
				},
			},
		},
		{
			name: "docker and bare refs",
			files: map[string][]byte{"w.yml": []byte(`steps:
  - uses: docker://alpine:3.20
  - uses: actions/checkout
`)},
			want: []Use{
				{File: "w.yml", Line: 2, Raw: "docker://alpine:3.20", Kind: UseDocker},
				{File: "w.yml", Line: 3, Raw: "actions/checkout", Slug: "actions/checkout", Kind: UseRemote},
			},
		},
		{
			name: "non-string and empty uses values are ignored",
			files: map[string][]byte{"w.yml": []byte(`steps:
  - uses:
      - a
      - b
  - uses:
  - uses: {}
`)},
			want: nil,
		},
		{
			name: "uses as a value never matches",
			files: map[string][]byte{"w.yml": []byte(`steps:
  - run: uses
  - name: uses
    with:
      script: "uses: actions/checkout@v4"
`)},
			want: nil,
		},
		{
			name:  "malformed yaml becomes an invalid entry",
			files: map[string][]byte{"broken.yml": []byte("jobs:\n  build:\n   - [unclosed\n")},
			want: []Use{
				{File: "broken.yml", Line: 1, Kind: UseInvalid, Err: "SENTINEL"},
			},
		},
		{
			name: "multi-document yaml",
			files: map[string][]byte{"multi.yml": []byte(`steps:
  - uses: actions/checkout@v4
---
steps:
  - uses: actions/cache@v3
`)},
			want: []Use{
				{File: "multi.yml", Line: 2, Raw: "actions/checkout@v4", Slug: "actions/checkout", Ref: "v4", Kind: UseRemote},
				{File: "multi.yml", Line: 5, Raw: "actions/cache@v3", Slug: "actions/cache", Ref: "v3", Kind: UseRemote},
			},
		},
		{
			name: "a broken later document keeps the earlier ones",
			files: map[string][]byte{"multi.yml": []byte(`steps:
  - uses: actions/checkout@v4
---
- [unclosed
`)},
			want: []Use{
				{File: "multi.yml", Line: 1, Kind: UseInvalid, Err: "SENTINEL"},
				{File: "multi.yml", Line: 2, Raw: "actions/checkout@v4", Slug: "actions/checkout", Ref: "v4", Kind: UseRemote},
			},
		},
		{
			name: "comment variants",
			files: map[string][]byte{"w.yml": []byte(`steps:
  - uses: a/b@` + strings.Repeat("a", 40) + ` #    v1.2.3 pinned by shuck
  - uses: a/c@v1 # not-a-version
`)},
			want: []Use{
				{
					File: "w.yml", Line: 2,
					Raw:  "a/b@" + strings.Repeat("a", 40),
					Slug: "a/b", Ref: strings.Repeat("a", 40),
					Comment: "v1.2.3 pinned by shuck", Kind: UseRemote,
				},
				{File: "w.yml", Line: 3, Raw: "a/c@v1", Slug: "a/c", Ref: "v1", Comment: "not-a-version", Kind: UseRemote},
			},
		},
		{
			name: "several files are ordered by path then line",
			files: map[string][]byte{
				"b.yml": []byte("steps:\n  - uses: a/b@v1\n"),
				"a.yml": []byte("steps:\n  - uses: a/a@v1\n"),
			},
			want: []Use{
				{File: "a.yml", Line: 2, Raw: "a/a@v1", Slug: "a/a", Ref: "v1", Kind: UseRemote},
				{File: "b.yml", Line: 2, Raw: "a/b@v1", Slug: "a/b", Ref: "v1", Kind: UseRemote},
			},
		},
		{
			name:  "empty input",
			files: map[string][]byte{"empty.yml": []byte("")},
			want:  nil,
		},
		{
			name:  "aliases are not followed",
			files: map[string][]byte{"w.yml": []byte("a: &x\n  uses: a/b@v1\nb: *x\n")},
			want: []Use{
				{File: "w.yml", Line: 2, Raw: "a/b@v1", Slug: "a/b", Ref: "v1", Kind: UseRemote},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scan(tt.files)
			if len(got) != len(tt.want) {
				t.Fatalf("Scan returned %d uses, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				// The parse error text comes from yaml.v3; assert it is present
				// and mentions the file rather than pinning its wording.
				if want.Err == "SENTINEL" {
					if !strings.Contains(got[i].Err, want.File) {
						t.Errorf("use %d: Err = %q, want it to name %q", i, got[i].Err, want.File)
					}
					want.Err = got[i].Err
				}
				if !reflect.DeepEqual(got[i], want) {
					t.Errorf("use %d:\n got %+v\nwant %+v", i, got[i], want)
				}
			}
		})
	}
}

func TestScanEmptyInput(t *testing.T) {
	if got := Scan(nil); got != nil {
		t.Fatalf("Scan(nil) = %+v, want nil", got)
	}
}

func TestIsSHA(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{strings.Repeat("a", 40), true},
		{strings.Repeat("0", 64), true},
		{"11bd71901bbe5b1630ceea73d27597364c9af683", true},
		{strings.Repeat("A", 40), false}, // uppercase is not the convention git writes
		{strings.Repeat("a", 39), false}, // abbreviated SHAs are not immutable refs
		{strings.Repeat("g", 40), false},
		{"v4.2.2", false},
		{"main", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			if got := IsSHA(tt.ref); got != tt.want {
				t.Errorf("IsSHA(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestUseKindString(t *testing.T) {
	tests := []struct {
		kind UseKind
		want string
	}{
		{UseRemote, "remote"},
		{UseLocal, "local"},
		{UseDocker, "docker"},
		{UseInvalid, "invalid"},
		{UseKind(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeFiles materializes a path -> content map under dir, creating parents.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

func TestWorkflowFiles(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		dirs  []string
		want  []string
	}{
		{
			name: "workflows, root manifest and nested actions",
			files: map[string]string{
				".github/workflows/ci.yml":            "a",
				".github/workflows/release.yaml":      "b",
				".github/workflows/notes.md":          "c",
				".github/workflows/nested/deep.yml":   "d",
				"action.yml":                          "e",
				".github/actions/setup/action.yml":    "f",
				".github/actions/other/action.yaml":   "g",
				".github/actions/other/helper.yml":    "h",
				".github/actions/deep/sub/action.yml": "i",
			},
			want: []string{
				".github/actions/other/action.yaml",
				".github/actions/setup/action.yml",
				".github/workflows/ci.yml",
				".github/workflows/release.yaml",
				"action.yml",
			},
		},
		{
			name:  "both root manifest spellings",
			files: map[string]string{"action.yaml": "a"},
			want:  []string{"action.yaml"},
		},
		{
			name:  "missing directories are not an error",
			files: map[string]string{"README.md": "hi"},
			want:  nil,
		},
		{
			name:  "empty root",
			files: nil,
			want:  nil,
		},
		{
			name:  "a workflows directory entry that is itself a directory is skipped",
			files: map[string]string{".github/workflows/sub.yml/keep.txt": "x"},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, tt.files)

			got, err := WorkflowFiles(root)
			if err != nil {
				t.Fatalf("WorkflowFiles: %v", err)
			}
			var names []string
			for name := range got {
				names = append(names, name)
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("WorkflowFiles = %v, want %v", names, tt.want)
			}
			for _, name := range names {
				if len(got[name]) == 0 {
					t.Errorf("%s: contents were not read", name)
				}
			}
		})
	}
}

func TestWorkflowFilesMissingRoot(t *testing.T) {
	got, err := WorkflowFiles(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("WorkflowFiles on a missing root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("WorkflowFiles = %v, want empty", got)
	}
}

func TestWorkflowFilesSkipsOversizedFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".github/workflows/big.yml":   strings.Repeat("x", maxWorkflowFileSize+1),
		".github/workflows/small.yml": "steps: []",
	})

	got, err := WorkflowFiles(root)
	if err != nil {
		t.Fatalf("WorkflowFiles: %v", err)
	}
	if _, ok := got[".github/workflows/big.yml"]; ok {
		t.Error("an oversized workflow was read")
	}
	if _, ok := got[".github/workflows/small.yml"]; !ok {
		t.Error("the small workflow was not read")
	}
}

func TestWorkflowFilesReportsUnreadableDirectory(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		// A regular file where a directory is expected is a genuine error, not
		// the "this repo has no workflows" case that must degrade to empty.
		{"workflows is a file", ".github/workflows"},
		{"actions is a file", ".github/actions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{tt.path: "not a directory"})
			if _, err := WorkflowFiles(root); err == nil {
				t.Fatalf("WorkflowFiles accepted a non-directory %s", tt.path)
			}
		})
	}
}
