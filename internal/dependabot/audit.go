package dependabot

import (
	"fmt"
	"sort"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// Input bundles everything Audit needs: the parsed config (and whether it was
// present at all), the ecosystems detected in the repo, and the tuning knobs.
type Input struct {
	Owner        string
	Repo         string
	ConfigSource string // a path or github: ref; empty when no config exists
	HasConfig    bool
	Config       Config
	Detected     []Detected

	// Misnamed is set when the config was found at .github/dependabot.yaml
	// (GitHub only reads the .yml spelling), so the audit can flag it.
	Misnamed bool
	// ErrorOnMissingEcosystem promotes an uncovered-ecosystem finding from a
	// warning to an error, so --exit-code gates on missing coverage.
	ErrorOnMissingEcosystem bool
}

// Audit evaluates a repository's Dependabot setup: which ecosystems it uses,
// whether the config covers them, and whether each update entry follows the
// best practices (grouping, assignees, labels, cooldowns, PR limits). It is
// pure — callers fetch the inputs via the gh layer and render the report.
func Audit(in Input) *model.DependabotReport {
	rep := &model.DependabotReport{
		Owner:        in.Owner,
		Repo:         in.Repo,
		ConfigSource: in.ConfigSource,
		HasConfig:    in.HasConfig,
	}

	covered, cfgDirs, cfgGlob := configIndex(in.Config)
	rep.Detected = ecosystems(in.Detected, covered)

	a := &auditor{rep: rep}

	a.configFindings(in)
	a.coverageFindings(in, cfgDirs, cfgGlob)
	a.undetectedFindings(in, covered)
	if in.HasConfig {
		a.bestPracticeFindings(in.Config)
	}

	return rep
}

// auditor accumulates findings onto a report.
type auditor struct{ rep *model.DependabotReport }

func (a *auditor) add(f model.DependabotFinding) { a.rep.Findings = append(a.rep.Findings, f) }

// configFindings reports problems with the file itself: absent, or mislocated.
func (a *auditor) configFindings(in Input) {
	if !in.HasConfig {
		msg := "no .github/dependabot.yml found"
		sug := "add .github/dependabot.yml to keep dependencies up to date"
		if len(in.Detected) > 0 {
			sug = fmt.Sprintf("run `shuck dependabot discover` to scaffold one for the %d ecosystem(s) detected", len(in.Detected))
		}
		a.add(model.DependabotFinding{
			Level: model.DependabotError, Category: model.DependabotCategoryConfig,
			Message: msg, Suggestion: sug,
		})
		return
	}
	if in.Misnamed {
		a.add(model.DependabotFinding{
			Level: model.DependabotWarning, Category: model.DependabotCategoryConfig,
			Message:    "config is at .github/dependabot.yaml, which GitHub ignores",
			Suggestion: "rename it to .github/dependabot.yml (the .yml spelling)",
		})
	}
}

// coverageFindings reports detected ecosystems with no update entry, and
// detected directories an otherwise-covered ecosystem omits.
func (a *auditor) coverageFindings(in Input, cfgDirs map[string][]string, cfgGlob map[string]bool) {
	missLevel := model.DependabotWarning
	if in.ErrorOnMissingEcosystem {
		missLevel = model.DependabotError
	}
	for _, d := range in.Detected {
		if len(cfgDirs[d.Ecosystem]) == 0 && !covers(cfgDirs, d.Ecosystem) {
			a.add(model.DependabotFinding{
				Level: missLevel, Category: model.DependabotCategoryCoverage, Ecosystem: d.Ecosystem,
				Directory:  strings.Join(d.Directories, ", "),
				Message:    fmt.Sprintf("%s is used (manifests in %s) but has no update entry", d.Ecosystem, strings.Join(d.Directories, ", ")),
				Suggestion: fmt.Sprintf("add an updates entry with package-ecosystem: %s", d.Ecosystem),
			})
			continue
		}
		// Covered ecosystem: flag any detected directory the config omits, unless
		// it uses a glob directory (which may match without an exact listing).
		if cfgGlob[d.Ecosystem] {
			continue
		}
		have := toSet(cfgDirs[d.Ecosystem])
		for _, dir := range d.Directories {
			if !have[dir] {
				a.add(model.DependabotFinding{
					Level: model.DependabotInfo, Category: model.DependabotCategoryCoverage, Ecosystem: d.Ecosystem, Directory: dir,
					Message:    fmt.Sprintf("%s has a manifest in %s that no update entry covers", d.Ecosystem, dir),
					Suggestion: fmt.Sprintf("add %s to the %s entry's directories", dir, d.Ecosystem),
				})
			}
		}
	}
}

// undetectedFindings notes config entries whose ecosystem shuck did not find in
// the repo — a removed manifest, a path shuck does not scan, or a typo.
func (a *auditor) undetectedFindings(in Input, covered map[string]bool) {
	detected := map[string]bool{}
	for _, d := range in.Detected {
		detected[d.Ecosystem] = true
	}
	var orphans []string
	for eco := range covered {
		if !detected[eco] {
			orphans = append(orphans, eco)
		}
	}
	sort.Strings(orphans)
	for _, eco := range orphans {
		a.add(model.DependabotFinding{
			Level: model.DependabotInfo, Category: model.DependabotCategoryCoverage, Ecosystem: eco,
			Message:    fmt.Sprintf("%s has an update entry but no matching manifest was detected", eco),
			Suggestion: "confirm the directory is correct, or remove the entry if the dependency was dropped",
		})
	}
}

// bestPracticeFindings flags update entries missing recommended settings:
// grouping, assignees/reviewers, labels, a cooldown, an open-PR limit, and a
// commit-message prefix.
func (a *auditor) bestPracticeFindings(cfg Config) {
	for _, u := range cfg.Updates {
		eco := u.PackageEcosystem
		dir := firstDir(u)
		bp := func(level model.DependabotLevel, msg, sug string) {
			a.add(model.DependabotFinding{
				Level: level, Category: model.DependabotCategoryBestPractice,
				Ecosystem: eco, Directory: dir, Message: msg, Suggestion: sug,
			})
		}
		if len(u.Groups) == 0 {
			bp(model.DependabotWarning, "no groups — every dependency opens its own PR",
				"add a group (e.g. patterns: [\"*\"], update-types: [minor, patch]) to batch updates")
		}
		if !u.hasAssignment() {
			bp(model.DependabotWarning, "no assignees or reviewers — update PRs may go unnoticed",
				"add assignees so the PRs land on someone's plate")
		}
		if len(u.Labels) == 0 {
			bp(model.DependabotInfo, "no labels — PRs are harder to triage and automate",
				"add labels (e.g. [dependencies]) for filtering and automation")
		}
		if u.Cooldown == nil {
			bp(model.DependabotInfo, "no cooldown — PRs open the moment a release lands",
				"add a cooldown (a minimum release age) to skip brand-new, unproven releases")
		}
		if u.OpenPullRequestsLimit == nil {
			bp(model.DependabotInfo, "open-pull-requests-limit not set (defaults to 5)",
				"set open-pull-requests-limit explicitly to control PR volume")
		}
		if u.CommitMessage == nil || u.CommitMessage.Prefix == "" {
			bp(model.DependabotInfo, "no commit-message prefix",
				"set commit-message.prefix for conventional commits and changelog grouping")
		}
	}
}

// configIndex builds, from a config, the set of covered ecosystems, each
// ecosystem's declared directories, and whether any of those use a glob.
func configIndex(cfg Config) (covered map[string]bool, dirs map[string][]string, glob map[string]bool) {
	covered = map[string]bool{}
	dirs = map[string][]string{}
	glob = map[string]bool{}
	for _, u := range cfg.Updates {
		eco := u.PackageEcosystem
		covered[eco] = true
		for _, d := range u.dirs() {
			dirs[eco] = append(dirs[eco], d)
			if strings.ContainsAny(d, "*?[") {
				glob[eco] = true
			}
		}
	}
	return covered, dirs, glob
}

// covers reports whether the config has any directory entry for the ecosystem
// (i.e. it is managed at all), distinct from the empty-slice "not present" case.
func covers(dirs map[string][]string, eco string) bool {
	_, ok := dirs[eco]
	return ok
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// firstDir returns a representative directory label for an update entry.
func firstDir(u Update) string {
	d := u.dirs()
	if len(d) == 0 {
		return ""
	}
	if len(d) == 1 {
		return d[0]
	}
	return strings.Join(d, ", ")
}
