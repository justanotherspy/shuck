package action

import (
	"encoding/json"
	"fmt"
	"io"
)

// SchemaVersion is the version of the JSON document `shuck action --json`
// emits. It is bumped only on a breaking change; additive fields keep it.
const SchemaVersion = 1

// jsonDoc is the stable, machine-readable shape of a resolved pin.
type jsonDoc struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	Subpath       string `json:"subpath,omitempty"`
	Requested     string `json:"requested"`
	Tag           string `json:"tag"`
	SHA           string `json:"sha"`
	Ref           string `json:"ref"`
	Pin           string `json:"pin"`
}

// Render writes the resolved pin as human-readable, multi-line detail.
func Render(w io.Writer, r Resolved) {
	fmt.Fprintln(w, r.Ref.Slug())
	fmt.Fprintf(w, "  tag: %s\n", r.Tag)
	fmt.Fprintf(w, "  sha: %s\n", r.SHA)
	fmt.Fprintf(w, "  pin: %s\n", r.PinLine())
}

// EncodeJSON writes the resolved pin as an indented JSON document with a
// trailing newline.
func EncodeJSON(w io.Writer, r Resolved) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonDoc{
		SchemaVersion: SchemaVersion,
		Action:        r.Ref.Slug(),
		Owner:         r.Ref.Owner,
		Repo:          r.Ref.Repo,
		Subpath:       r.Ref.Subpath,
		Requested:     r.Ref.Constraint,
		Tag:           r.Tag,
		SHA:           r.SHA,
		Ref:           r.UsesRef(),
		Pin:           r.PinLine(),
	})
}
