package image

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestEncodeListJSON(t *testing.T) {
	pkgs := []model.ImagePackage{
		{Owner: "acme", Name: "api", Versions: []model.ImageVersion{
			{Tags: []string{"v2.0.0", "latest"}, Digest: "sha256:aaa", UpdatedAt: time.Unix(1000, 0)},
		}},
		{Owner: "acme", Name: "worker", Versions: nil}, // no versions
	}
	var b bytes.Buffer
	if err := EncodeListJSON(&b, "ghcr.io", "acme", pkgs); err != nil {
		t.Fatalf("EncodeListJSON: %v", err)
	}
	var doc ListDocument
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b.String())
	}
	if doc.SchemaVersion != SchemaVersion || doc.Registry != "ghcr.io" || doc.Owner != "acme" {
		t.Errorf("doc header = %+v", doc)
	}
	if len(doc.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(doc.Images))
	}
	if doc.Images[0].LatestTag != "v2.0.0" || doc.Images[0].Digest != "sha256:aaa" {
		t.Errorf("api image = %+v", doc.Images[0])
	}
	if doc.Images[0].UpdatedAt == "" {
		t.Errorf("expected an UpdatedAt timestamp, got empty")
	}
	if doc.Images[1].LatestTag != "" || doc.Images[1].Versions != 0 {
		t.Errorf("empty worker image = %+v", doc.Images[1])
	}
}

func TestEncodeListJSONEmpty(t *testing.T) {
	var b bytes.Buffer
	if err := EncodeListJSON(&b, "ghcr.io", "acme", nil); err != nil {
		t.Fatalf("EncodeListJSON: %v", err)
	}
	// Images is always an empty slice, never null.
	if !strings.Contains(b.String(), `"images": []`) {
		t.Errorf("expected empty images array, got:\n%s", b.String())
	}
}

func TestSelectNewestPicksFirstTagWhenNoLatest(t *testing.T) {
	// No semver tags and no "latest" tag: the newest-by-UpdatedAt version is
	// chosen and its first tag is used.
	versions := []model.ImageVersion{
		{Tags: []string{"old-edge"}, Digest: "sha256:old", UpdatedAt: time.Unix(10, 0)},
		{Tags: []string{"nightly", "rolling"}, Digest: "sha256:new", UpdatedAt: time.Unix(20, 0)},
	}
	v, tag, err := Select(versions, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if v.Digest != "sha256:new" || tag != "nightly" {
		t.Errorf("Select = %q/%q, want sha256:new/nightly", v.Digest, tag)
	}
}

func TestSelectNewestPrefersLatestTag(t *testing.T) {
	// Non-semver tags where "latest" is present but not first: selectNewest
	// promotes the "latest" tag over the first one.
	versions := []model.ImageVersion{
		{Tags: []string{"edge", "latest"}, Digest: "sha256:x", UpdatedAt: time.Unix(5, 0)},
	}
	v, tag, err := Select(versions, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if v.Digest != "sha256:x" || tag != "latest" {
		t.Errorf("Select = %q/%q, want sha256:x/latest", v.Digest, tag)
	}
}

func TestSelectNewestSkipsUntaggedVersions(t *testing.T) {
	// Versions with no tags are ignored; only the tagged one is selectable.
	versions := []model.ImageVersion{
		{Tags: nil, Digest: "sha256:untagged", UpdatedAt: time.Unix(99, 0)},
		{Tags: []string{"edge"}, Digest: "sha256:tagged", UpdatedAt: time.Unix(1, 0)},
	}
	v, tag, err := Select(versions, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if v.Digest != "sha256:tagged" || tag != "edge" {
		t.Errorf("Select = %q/%q, want sha256:tagged/edge", v.Digest, tag)
	}
}

func TestSelectNewestAllUntagged(t *testing.T) {
	versions := []model.ImageVersion{{Tags: nil, Digest: "sha256:x"}}
	if _, _, err := Select(versions, ""); err == nil {
		t.Error("expected error when no version carries a tag")
	}
}

// referrerTag is a syntactically valid cosign/OCI referrer tag (sha256- + 64
// hex chars) used by the referrer-exclusion tests below.
const referrerTag = "sha256-618acdb4607c26dda5747aaf0ca4ab8b6768621754701e94936288f5a6886739"

func TestSelectNewestSkipsReferrerArtifacts(t *testing.T) {
	// A cosign signature/attestation artifact is pushed *after* the image it
	// signs, so it is always the most recently updated version. It must never
	// be selected as "latest" — the real image (here tagged "edge") must win.
	versions := []model.ImageVersion{
		{Tags: []string{"edge", "sha-35b0bc5"}, Digest: "sha256:image", UpdatedAt: time.Unix(10, 0)},
		{Tags: []string{referrerTag}, Digest: "sha256:signature", UpdatedAt: time.Unix(20, 0)},
	}
	v, tag, err := Select(versions, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if v.Digest != "sha256:image" || tag != "edge" {
		t.Errorf("Select = %q/%q, want sha256:image/edge", v.Digest, tag)
	}
}

func TestSelectNewestSkipsReferrerTagWithinVersion(t *testing.T) {
	// A version carrying both a referrer tag and a real tag is selectable, but
	// the referrer tag is never the one used to name it.
	versions := []model.ImageVersion{
		{Tags: []string{referrerTag, "nightly"}, Digest: "sha256:image", UpdatedAt: time.Unix(10, 0)},
	}
	v, tag, err := Select(versions, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if v.Digest != "sha256:image" || tag != "nightly" {
		t.Errorf("Select = %q/%q, want sha256:image/nightly", v.Digest, tag)
	}
}

func TestSelectNewestAllReferrersError(t *testing.T) {
	// A package holding only referrer artifacts has nothing pinnable.
	versions := []model.ImageVersion{
		{Tags: []string{referrerTag}, Digest: "sha256:signature", UpdatedAt: time.Unix(20, 0)},
	}
	if _, _, err := Select(versions, ""); err == nil {
		t.Error("expected error when every version is a referrer artifact")
	}
}

func TestSelectExactReferrerTagStillResolves(t *testing.T) {
	// An explicitly requested referrer tag is honored — exclusion only applies
	// to the "pick the latest" fallback.
	versions := []model.ImageVersion{
		{Tags: []string{referrerTag}, Digest: "sha256:signature", UpdatedAt: time.Unix(20, 0)},
	}
	v, tag, err := Select(versions, referrerTag)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if v.Digest != "sha256:signature" || tag != referrerTag {
		t.Errorf("Select = %q/%q, want sha256:signature/%s", v.Digest, tag, referrerTag)
	}
}

func TestIsReferrerTag(t *testing.T) {
	for _, tt := range []struct {
		tag  string
		want bool
	}{
		{referrerTag, true},
		{referrerTag + ".sig", true},
		{referrerTag + ".att", true},
		{referrerTag + ".sbom", true},
		{"latest", false},
		{"edge", false},
		{"v1.2.3", false},
		{"sha-35b0bc5", false},                       // commit-SHA image tag, not a referrer
		{"sha256-abc", false},                        // too short
		{"sha256-" + strings.Repeat("z", 64), false}, // not hex
		{"sha256:" + strings.Repeat("a", 64), false}, // digest, not a tag
		{"", false},
	} {
		if got := IsReferrerTag(tt.tag); got != tt.want {
			t.Errorf("IsReferrerTag(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}

func TestLatestVersionFalseOnEmpty(t *testing.T) {
	if _, _, ok := LatestVersion(nil); ok {
		t.Error("LatestVersion(nil) ok=true, want false")
	}
}

func TestParseRefRegistryWithPort(t *testing.T) {
	ref, err := ParseRef("registry.example.com:5000/acme/api:v1")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Registry != "registry.example.com:5000" || ref.Owner != "acme" || ref.Name != "api" || ref.Constraint != "v1" {
		t.Errorf("ParseRef = %+v", ref)
	}
}

func TestParseRefTrailingSlashIsBareOwner(t *testing.T) {
	// A trailing slash is trimmed, leaving a bare owner (list-all).
	ref, err := ParseRef("acme/")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Owner != "acme" || ref.Name != "" || !ref.ListAll() {
		t.Errorf("ParseRef(acme/) = %+v, want bare owner acme", ref)
	}
}

func TestParseRefColonInPathNotTreatedAsTag(t *testing.T) {
	// A ':' that is followed by a '/' is not a tag separator (defensive branch).
	ref, err := ParseRef("ghcr.io/acme/weird:thing/more")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Constraint != "" {
		t.Errorf("ParseRef colon-in-path constraint = %q, want empty", ref.Constraint)
	}
	if ref.Name != "weird:thing/more" {
		t.Errorf("ParseRef name = %q", ref.Name)
	}
}

func TestParseRefWhitespaceOnly(t *testing.T) {
	if _, err := ParseRef("   "); err == nil {
		t.Error("expected error for a whitespace-only ref")
	}
}
