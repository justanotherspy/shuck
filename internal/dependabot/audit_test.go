package dependabot

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// goodUpdate is an entry that satisfies every best-practice check.
func goodUpdate(eco string, dirs ...string) Update {
	u := bestPracticeUpdate(eco, dirs)
	u.Assignees = []string{"alice"}
	return u
}

func findCategory(r *model.DependabotReport, cat string) []model.DependabotFinding {
	var out []model.DependabotFinding
	for _, f := range r.Findings {
		if f.Category == cat {
			out = append(out, f)
		}
	}
	return out
}

func hasFinding(r *model.DependabotReport, level model.DependabotLevel, eco, substr string) bool {
	for _, f := range r.Findings {
		if f.Level == level && f.Ecosystem == eco && strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

func TestAuditNoConfig(t *testing.T) {
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: false,
		Detected: []Detected{{Ecosystem: "gomod", Directories: []string{"/"}}},
	})
	if !r.HasErrors() {
		t.Fatal("missing config should be an error")
	}
	if len(findCategory(r, model.DependabotCategoryConfig)) != 1 {
		t.Errorf("want 1 config finding, got %d", len(findCategory(r, model.DependabotCategoryConfig)))
	}
	// The uncovered ecosystem is a warning by default.
	if !hasFinding(r, model.DependabotWarning, "gomod", "no update entry") {
		t.Errorf("missing coverage warning: %+v", r.Findings)
	}
	if r.HasConfig {
		t.Error("HasConfig should be false")
	}
}

func TestAuditErrorOnMissingEcosystem(t *testing.T) {
	cfg := Config{Version: new(2), Updates: []Update{goodUpdate("gomod", "/")}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg,
		Detected: []Detected{
			{Ecosystem: "gomod", Directories: []string{"/"}},
			{Ecosystem: "npm", Directories: []string{"/web"}},
		},
		ErrorOnMissingEcosystem: true,
	})
	if !hasFinding(r, model.DependabotError, "npm", "no update entry") {
		t.Errorf("uncovered npm should be an error with the flag: %+v", r.Findings)
	}
	if !r.HasErrors() {
		t.Error("HasErrors should be true")
	}
}

func TestAuditClean(t *testing.T) {
	good := goodUpdate("gomod", "/")
	good.Cooldown = &Cooldown{DefaultDays: new(5)}
	cfg := Config{Version: new(2), Updates: []Update{good}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg,
		Detected: []Detected{{Ecosystem: "gomod", Directories: []string{"/"}}},
	})
	if !r.OK() {
		t.Errorf("expected clean audit, got findings: %+v", r.Findings)
	}
	if len(r.Detected) != 1 || !r.Detected[0].Covered {
		t.Errorf("gomod should be covered: %+v", r.Detected)
	}
}

func TestAuditBestPracticeGaps(t *testing.T) {
	bare := Update{
		PackageEcosystem: "gomod", Directory: "/",
		Schedule: &Schedule{Interval: "weekly"},
	}
	cfg := Config{Version: new(2), Updates: []Update{bare}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg,
		Detected: []Detected{{Ecosystem: "gomod", Directories: []string{"/"}}},
	})
	if !hasFinding(r, model.DependabotWarning, "gomod", "no groups") {
		t.Error("expected groups warning")
	}
	if !hasFinding(r, model.DependabotWarning, "gomod", "no assignees") {
		t.Error("expected assignees warning")
	}
	if !hasFinding(r, model.DependabotInfo, "gomod", "no labels") {
		t.Error("expected labels info")
	}
	if !hasFinding(r, model.DependabotInfo, "gomod", "no cooldown") {
		t.Error("expected cooldown info")
	}
	if !hasFinding(r, model.DependabotInfo, "gomod", "open-pull-requests-limit") {
		t.Error("expected PR limit info")
	}
	if !hasFinding(r, model.DependabotInfo, "gomod", "no commit-message prefix") {
		t.Error("expected commit-message info")
	}
}

func TestAuditMisnamed(t *testing.T) {
	cfg := Config{Version: new(2), Updates: []Update{goodUpdate("gomod", "/")}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg, Misnamed: true,
		Detected: []Detected{{Ecosystem: "gomod", Directories: []string{"/"}}},
	})
	found := false
	for _, f := range findCategory(r, model.DependabotCategoryConfig) {
		if f.Level == model.DependabotWarning && strings.Contains(f.Message, ".yaml") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected misnamed warning: %+v", r.Findings)
	}
}

func TestAuditDirectoryCoverage(t *testing.T) {
	cfg := Config{Version: new(2), Updates: []Update{goodUpdate("npm", "/web")}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg,
		Detected: []Detected{{Ecosystem: "npm", Directories: []string{"/web", "/api"}}},
	})
	if !hasFinding(r, model.DependabotInfo, "npm", "/api") {
		t.Errorf("expected directory-coverage info for /api: %+v", r.Findings)
	}
}

func TestAuditGlobSuppressesDirectoryFinding(t *testing.T) {
	u := goodUpdate("npm")
	u.Directory = ""
	u.Directories = []string{"/packages/*"}
	cfg := Config{Version: new(2), Updates: []Update{u}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg,
		Detected: []Detected{{Ecosystem: "npm", Directories: []string{"/packages/a", "/packages/b"}}},
	})
	for _, f := range r.Findings {
		if f.Category == model.DependabotCategoryCoverage && f.Level == model.DependabotInfo {
			t.Errorf("glob should suppress per-directory findings, got %+v", f)
		}
	}
}

func TestAuditUndetectedOrphan(t *testing.T) {
	cfg := Config{Version: new(2), Updates: []Update{goodUpdate("pip", "/")}}
	r := Audit(Input{
		Owner: "o", Repo: "r", HasConfig: true, Config: cfg,
		Detected: []Detected{{Ecosystem: "gomod", Directories: []string{"/"}}},
	})
	if !hasFinding(r, model.DependabotInfo, "pip", "no matching manifest") {
		t.Errorf("expected orphan info for pip: %+v", r.Findings)
	}
}
