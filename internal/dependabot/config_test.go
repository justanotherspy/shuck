package dependabot

import (
	"strings"
	"testing"
)

const validConfig = `version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
    assignees: [alice]
    labels: [dependencies]
    groups:
      all:
        patterns: ["*"]
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Version == nil || *cfg.Version != 2 {
		t.Fatalf("version = %v, want 2", cfg.Version)
	}
	if len(cfg.Updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(cfg.Updates))
	}
	u := cfg.Updates[0]
	if u.PackageEcosystem != "gomod" {
		t.Errorf("ecosystem = %q", u.PackageEcosystem)
	}
	if !u.hasAssignment() {
		t.Error("expected hasAssignment true")
	}
	if got := u.dirs(); len(got) != 1 || got[0] != "/" {
		t.Errorf("dirs = %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", "empty"},
		{"unknown field", "version: 2\nupdates: []\nbogus: 1\n", "field bogus"},
		{"unknown update field", "version: 2\nupdates:\n  - package-ecosystem: gomod\n    directory: /\n    schedule:\n      interval: weekly\n    typo: x\n", "field typo"},
		{"missing version", "updates: []\n", "missing the required 'version'"},
		{"bad version", "version: 1\nupdates: []\n", "unsupported dependabot version"},
		{"no updates", "version: 2\nupdates: []\n", "no updates"},
		{"missing ecosystem", "version: 2\nupdates:\n  - directory: /\n    schedule:\n      interval: weekly\n", "missing package-ecosystem"},
		{"unknown ecosystem", "version: 2\nupdates:\n  - package-ecosystem: cabal\n    directory: /\n    schedule:\n      interval: weekly\n", "unknown package-ecosystem"},
		{"missing directory", "version: 2\nupdates:\n  - package-ecosystem: gomod\n    schedule:\n      interval: weekly\n", "missing directory"},
		{"missing schedule", "version: 2\nupdates:\n  - package-ecosystem: gomod\n    directory: /\n", "missing schedule.interval"},
		{"bad interval", "version: 2\nupdates:\n  - package-ecosystem: gomod\n    directory: /\n    schedule:\n      interval: hourly\n", "invalid schedule.interval"},
		{"not yaml", "::: not yaml :::", "parse dependabot config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body))
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestParseDirectories(t *testing.T) {
	cfg, err := Parse([]byte(`version: 2
updates:
  - package-ecosystem: npm
    directories: ["/a", "/b/"]
    schedule:
      interval: daily
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Updates[0].dirs()
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("dirs = %v, want [/a /b]", got)
	}
	if cfg.Updates[0].hasAssignment() {
		t.Error("hasAssignment should be false")
	}
}
