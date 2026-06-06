package dependabot

import (
	"io"
	"testing"
)

// FuzzParse asserts Parse never panics on arbitrary input and that any config
// it accepts satisfies the invariants the audit relies on (version 2, at least
// one update, each update with a known ecosystem, a directory, and a valid
// schedule interval) and can be audited and re-rendered without panicking.
func FuzzParseDependabot(f *testing.F) {
	f.Add([]byte(validConfig))
	f.Add([]byte("version: 2\nupdates:\n  - package-ecosystem: npm\n    directories: [\"/a\"]\n    schedule:\n      interval: daily\n"))
	f.Add([]byte("version: 1\n"))
	f.Add([]byte(""))
	f.Add([]byte("not: yaml: ["))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16<<10 {
			return
		}
		cfg, err := Parse(data)
		if err != nil {
			return
		}
		if cfg.Version == nil || *cfg.Version != 2 {
			t.Fatalf("accepted config without version 2: %+v", cfg.Version)
		}
		if len(cfg.Updates) == 0 {
			t.Fatal("accepted config with no updates")
		}
		for i, u := range cfg.Updates {
			if u.PackageEcosystem == "" {
				t.Fatalf("updates[%d] has empty ecosystem", i)
			}
			if u.Directory == "" && len(u.Directories) == 0 {
				t.Fatalf("updates[%d] has no directory", i)
			}
			if u.Schedule == nil || u.Schedule.Interval == "" {
				t.Fatalf("updates[%d] has no schedule interval", i)
			}
		}
		// Audit and both renderers must tolerate any accepted config.
		rep := Audit(Input{Owner: "o", Repo: "r", HasConfig: true, Config: cfg})
		Render(io.Discard, rep)
		if err := EncodeJSON(io.Discard, rep); err != nil {
			t.Fatalf("EncodeJSON: %v", err)
		}
		// A parsed config must also round-trip through Discover's extend path.
		if _, err := Discover(data, nil); err != nil {
			t.Fatalf("Discover rejected an already-parsed config: %v", err)
		}
	})
}
