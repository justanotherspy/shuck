package image

import (
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in         string
		wantErr    bool
		registry   string
		owner      string
		name       string
		constraint string
	}{
		{in: "ghcr.io/acme/api", registry: "ghcr.io", owner: "acme", name: "api"},
		{in: "ghcr.io/acme/api:v1.2", registry: "ghcr.io", owner: "acme", name: "api", constraint: "v1.2"},
		{in: "ghcr.io/acme/api:latest", registry: "ghcr.io", owner: "acme", name: "api", constraint: "latest"},
		{in: "ghcr.io/acme/api@sha256:abc", registry: "ghcr.io", owner: "acme", name: "api", constraint: "sha256:abc"},
		{in: "acme/api", registry: "ghcr.io", owner: "acme", name: "api"},
		{in: "acme/team/api:v2", registry: "ghcr.io", owner: "acme", name: "team/api", constraint: "v2"},
		{in: "acme", registry: "ghcr.io", owner: "acme", name: ""}, // bare owner: list all
		{in: "", wantErr: true},
		{in: "acme:v1", wantErr: true}, // a tag needs a name
	}
	for _, tc := range tests {
		got, err := ParseRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRef(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q) err=%v", tc.in, err)
			continue
		}
		if got.Registry != tc.registry || got.Owner != tc.owner || got.Name != tc.name || got.Constraint != tc.constraint {
			t.Errorf("ParseRef(%q) = %+v, want registry=%q owner=%q name=%q constraint=%q",
				tc.in, got, tc.registry, tc.owner, tc.name, tc.constraint)
		}
	}
}

func TestRefListAllAndSlug(t *testing.T) {
	bare, _ := ParseRef("acme")
	if !bare.ListAll() {
		t.Errorf("bare owner ListAll() = false, want true")
	}
	if bare.Slug() != "ghcr.io/acme" {
		t.Errorf("bare Slug() = %q, want ghcr.io/acme", bare.Slug())
	}
	named, _ := ParseRef("ghcr.io/acme/api")
	if named.ListAll() {
		t.Errorf("named ListAll() = true, want false")
	}
	if named.Slug() != "ghcr.io/acme/api" {
		t.Errorf("named Slug() = %q, want ghcr.io/acme/api", named.Slug())
	}
}

func TestSelect(t *testing.T) {
	versions := []model.ImageVersion{
		{Tags: []string{"v4.2.2", "latest"}, Digest: "sha256:aaa", UpdatedAt: time.Unix(300, 0)},
		{Tags: []string{"v4.1.0"}, Digest: "sha256:bbb", UpdatedAt: time.Unix(200, 0)},
		{Tags: []string{"v3.0.0"}, Digest: "sha256:ccc", UpdatedAt: time.Unix(100, 0)},
		{Tags: []string{"v5.0.0-rc.1"}, Digest: "sha256:ddd", UpdatedAt: time.Unix(400, 0)},
	}
	tests := []struct {
		name       string
		versions   []model.ImageVersion
		constraint string
		wantDigest string
		wantTag    string
		wantErr    bool
	}{
		{name: "latest stable overall", versions: versions, constraint: "", wantDigest: "sha256:aaa", wantTag: "v4.2.2"},
		{name: "major constraint", versions: versions, constraint: "v3", wantDigest: "sha256:ccc", wantTag: "v3.0.0"},
		{name: "major.minor constraint", versions: versions, constraint: "v4.1", wantDigest: "sha256:bbb", wantTag: "v4.1.0"},
		{name: "exact tag latest", versions: versions, constraint: "latest", wantDigest: "sha256:aaa", wantTag: "latest"},
		{name: "no match", versions: versions, constraint: "v9", wantErr: true},
		{name: "unknown exact tag", versions: versions, constraint: "nope", wantErr: true},
		{
			name:       "prerelease fallback when no stable matches",
			versions:   []model.ImageVersion{{Tags: []string{"v5.0.0-rc.1"}, Digest: "sha256:ddd"}},
			constraint: "v5",
			wantDigest: "sha256:ddd",
			wantTag:    "v5.0.0-rc.1",
		},
		{
			name:       "non-semver newest by UpdatedAt",
			versions:   []model.ImageVersion{{Tags: []string{"nightly"}, Digest: "sha256:old", UpdatedAt: time.Unix(1, 0)}, {Tags: []string{"edge"}, Digest: "sha256:new", UpdatedAt: time.Unix(2, 0)}},
			constraint: "",
			wantDigest: "sha256:new",
			wantTag:    "edge",
		},
		{name: "empty list", versions: nil, constraint: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, tag, err := Select(tc.versions, tc.constraint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Select() err=nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Select() err=%v", err)
			}
			if v.Digest != tc.wantDigest {
				t.Errorf("digest = %q, want %q", v.Digest, tc.wantDigest)
			}
			if tag != tc.wantTag {
				t.Errorf("tag = %q, want %q", tag, tc.wantTag)
			}
		})
	}
}

func TestResolvedPinLine(t *testing.T) {
	ref, _ := ParseRef("ghcr.io/acme/api")
	r := Resolved{Ref: ref, Tag: "v1.2.3", Digest: "sha256:abc"}
	if got := r.PinRef(); got != "ghcr.io/acme/api@sha256:abc" {
		t.Errorf("PinRef() = %q", got)
	}
	if got := r.PinLine(); got != "ghcr.io/acme/api@sha256:abc # v1.2.3" {
		t.Errorf("PinLine() = %q", got)
	}
}
