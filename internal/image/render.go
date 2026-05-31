package image

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

// SchemaVersion is the version of the JSON documents `shuck image --json` emits.
// It is bumped only on a breaking change; additive fields keep it.
const SchemaVersion = 1

// Document is the stable, machine-readable shape of a single resolved image pin.
type Document struct {
	SchemaVersion int    `json:"schema_version"`
	Image         string `json:"image"` // registry/owner/name
	Registry      string `json:"registry"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Requested     string `json:"requested"`
	Tag           string `json:"tag"`
	Digest        string `json:"digest"`
	Ref           string `json:"ref"` // digest-pinned reference
	Pin           string `json:"pin"` // ref annotated with the tag
}

// PackageDoc is one image's summary within a list document.
type PackageDoc struct {
	Name      string `json:"name"`
	Image     string `json:"image"` // registry/owner/name
	LatestTag string `json:"latest_tag,omitempty"`
	Digest    string `json:"digest,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Versions  int    `json:"versions"`
}

// ListDocument is the stable shape of every image under an owner.
type ListDocument struct {
	SchemaVersion int          `json:"schema_version"`
	Registry      string       `json:"registry"`
	Owner         string       `json:"owner"`
	Images        []PackageDoc `json:"images"`
}

// Render writes a resolved image pin as human-readable, multi-line detail.
func Render(w io.Writer, r Resolved) {
	fmt.Fprintln(w, r.Ref.Slug())
	fmt.Fprintf(w, "  tag:    %s\n", r.Tag)
	fmt.Fprintf(w, "  digest: %s\n", r.Digest)
	fmt.Fprintf(w, "  pin:    %s\n", r.PinLine())
}

// RenderList writes every image under an owner as a one-line-per-image table.
func RenderList(w io.Writer, registry, owner string, pkgs []model.ImagePackage) {
	if len(pkgs) == 0 {
		fmt.Fprintf(w, "%s/%s: no container images\n", registry, owner)
		return
	}
	fmt.Fprintf(w, "%s/%s — %d image(s)\n", registry, owner, len(pkgs))
	for _, p := range pkgs {
		v, tag, ok := LatestVersion(p.Versions)
		if !ok {
			fmt.Fprintf(w, "  %s (no tagged versions)\n", p.Name)
			continue
		}
		fmt.Fprintf(w, "  %s\n", p.Name)
		fmt.Fprintf(w, "    tag:    %s\n", tag)
		fmt.Fprintf(w, "    digest: %s\n", v.Digest)
	}
}

// NewDocument projects a resolved image pin onto the stable JSON view.
func NewDocument(r Resolved) Document {
	return Document{
		SchemaVersion: SchemaVersion,
		Image:         r.Ref.Slug(),
		Registry:      r.Ref.Registry,
		Owner:         r.Ref.Owner,
		Name:          r.Ref.Name,
		Requested:     r.Ref.Constraint,
		Tag:           r.Tag,
		Digest:        r.Digest,
		Ref:           r.PinRef(),
		Pin:           r.PinLine(),
	}
}

// NewListDocument projects every image under an owner onto the stable JSON view.
func NewListDocument(registry, owner string, pkgs []model.ImagePackage) ListDocument {
	doc := ListDocument{SchemaVersion: SchemaVersion, Registry: registry, Owner: owner}
	doc.Images = make([]PackageDoc, 0, len(pkgs))
	for _, p := range pkgs {
		pd := PackageDoc{
			Name:     p.Name,
			Image:    registry + "/" + owner + "/" + p.Name,
			Versions: len(p.Versions),
		}
		if v, tag, ok := LatestVersion(p.Versions); ok {
			pd.LatestTag = tag
			pd.Digest = v.Digest
			if !v.UpdatedAt.IsZero() {
				pd.UpdatedAt = v.UpdatedAt.UTC().Format(time.RFC3339)
			}
		}
		doc.Images = append(doc.Images, pd)
	}
	return doc
}

// EncodeJSON writes a resolved image pin as indented JSON with a trailing
// newline.
func EncodeJSON(w io.Writer, r Resolved) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewDocument(r))
}

// EncodeListJSON writes every image under an owner as indented JSON.
func EncodeListJSON(w io.Writer, registry, owner string, pkgs []model.ImagePackage) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(NewListDocument(registry, owner, pkgs))
}
