package image

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestRenderAndDocument(t *testing.T) {
	ref, _ := ParseRef("ghcr.io/acme/api:v1")
	r := Resolved{Ref: ref, Tag: "v1.2.3", Digest: "sha256:abc"}

	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	for _, want := range []string{"ghcr.io/acme/api", "v1.2.3", "sha256:abc", "# v1.2.3"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render output missing %q:\n%s", want, out)
		}
	}

	doc := NewDocument(r)
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("schema = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if doc.Image != "ghcr.io/acme/api" || doc.Tag != "v1.2.3" || doc.Digest != "sha256:abc" {
		t.Errorf("doc = %+v", doc)
	}
	if doc.Requested != "v1" {
		t.Errorf("requested = %q, want v1", doc.Requested)
	}
	if doc.Ref != "ghcr.io/acme/api@sha256:abc" {
		t.Errorf("ref = %q", doc.Ref)
	}

	var jb bytes.Buffer
	if err := EncodeJSON(&jb, r); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var round Document
	if err := json.Unmarshal(jb.Bytes(), &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round != doc {
		t.Errorf("round-trip mismatch: %+v vs %+v", round, doc)
	}
}

func TestListDocument(t *testing.T) {
	pkgs := []model.ImagePackage{
		{Owner: "acme", Name: "api", Versions: []model.ImageVersion{
			{Tags: []string{"v2.0.0", "latest"}, Digest: "sha256:aaa", UpdatedAt: time.Unix(10, 0)},
			{Tags: []string{"v1.0.0"}, Digest: "sha256:bbb", UpdatedAt: time.Unix(5, 0)},
		}},
		{Owner: "acme", Name: "worker", Versions: nil},
	}

	doc := NewListDocument("ghcr.io", "acme", pkgs)
	if doc.Owner != "acme" || len(doc.Images) != 2 {
		t.Fatalf("doc = %+v", doc)
	}
	api := doc.Images[0]
	if api.Name != "api" || api.LatestTag != "v2.0.0" || api.Digest != "sha256:aaa" || api.Versions != 2 {
		t.Errorf("api package = %+v", api)
	}
	if api.Image != "ghcr.io/acme/api" {
		t.Errorf("api image = %q", api.Image)
	}
	worker := doc.Images[1]
	if worker.LatestTag != "" || worker.Digest != "" || worker.Versions != 0 {
		t.Errorf("empty worker package = %+v", worker)
	}

	var b bytes.Buffer
	RenderList(&b, "ghcr.io", "acme", pkgs)
	out := b.String()
	for _, want := range []string{"acme", "api", "sha256:aaa", "v2.0.0", "worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderList missing %q:\n%s", want, out)
		}
	}
}

func TestRenderListEmpty(t *testing.T) {
	var b bytes.Buffer
	RenderList(&b, "ghcr.io", "acme", nil)
	if !strings.Contains(b.String(), "no container images") {
		t.Errorf("empty list output = %q", b.String())
	}
}
