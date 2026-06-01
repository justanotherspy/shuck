package mcp

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestIsImageRef(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ghcr.io/owner/name", true},
		{"ghcr.io/owner/name:v1.2", true},
		{"GHCR.IO/owner/name", true}, // case-insensitive registry
		{"https://ghcr.io/owner/name", true},
		{"docker://ghcr.io/owner/name", true},
		{"owner", false},
		{"owner/repo", false},
		{"https://github.com/owner/repo", false},
		{"", false},
		{"  ghcr.io/o/n  ", true}, // trimmed
	}
	for _, c := range cases {
		if got := isImageRef(c.in); got != c.want {
			t.Errorf("isImageRef(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestImageOwnerNoNetwork(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"bare owner", "acme", "acme", false},
		{"trimmed bare owner", "  acme  ", "acme", false},
		{"owner/repo", "acme/api", "acme", false},
		{"github url", "https://github.com/acme/api", "acme", false},
		{"repo url no scheme", "github.com/acme/api", "acme", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := imageOwner(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Errorf("imageOwner(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestExtractInputApplyAll exercises every pointer override branch of apply.
func TestExtractInputApplyAll(t *testing.T) {
	c, s, tl, m := 1, 2, 3, 4
	in := extractInput{
		Context:         &c,
		ShortThreshold:  &s,
		Tail:            &tl,
		MaxCommandLines: &m,
		Pattern:         "boom",
		Full:            true,
	}
	got := in.apply(defaultOptions())
	if got.Context != 1 || got.ShortThreshold != 2 || got.Tail != 3 || got.MaxCommandLines != 4 {
		t.Errorf("sizing knobs not applied: %+v", got)
	}
	if got.Pattern != "boom" || !got.Full {
		t.Errorf("pattern/full not applied: %+v", got)
	}
}

func TestInspectSecurityTargetArgs(t *testing.T) {
	cases := []struct {
		name string
		in   inspectSecurityInput
		want []string
	}{
		{"url wins", inspectSecurityInput{URL: "https://github.com/o/r", Repo: "x/y"}, []string{"https://github.com/o/r"}},
		{"repo", inspectSecurityInput{Repo: "o/r"}, []string{"o/r"}},
		{"nothing", inspectSecurityInput{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.targetArgs()
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("targetArgs() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestToSecurityResult(t *testing.T) {
	report := &model.SecurityReport{
		Owner: "acme",
		Repo:  "api",
		State: "open",
		DependabotAlerts: []model.DependabotAlert{
			{Number: 1, State: "open", Severity: model.SeverityHigh, Package: "left-pad"},
		},
	}
	res, doc, err := toSecurityResult(report)
	if err != nil {
		t.Fatalf("toSecurityResult: %v", err)
	}
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("want 1 content block, got %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "acme/api") {
		t.Errorf("text content missing repo: %+v", res.Content[0])
	}
	if doc.Repo.Owner != "acme" || doc.Repo.Repo != "api" {
		t.Errorf("document repo = %+v, want acme/api", doc.Repo)
	}
	if doc.Summary.Total != 1 {
		t.Errorf("summary total = %d, want 1", doc.Summary.Total)
	}
}
