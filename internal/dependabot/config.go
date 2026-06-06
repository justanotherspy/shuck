// Package dependabot audits a repository's .github/dependabot.yml against the
// ecosystems it actually uses and a set of best practices (grouping, assignees,
// labels, cooldowns, schedules, PR limits). It does no network I/O: the gh layer
// fetches the repo's file tree and config, this package parses the config,
// detects ecosystems from the file list, and evaluates the two into a
// model.DependabotReport for text or JSON output. It can also generate or extend
// a best-practice config (Discover).
package dependabot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the parsed .github/dependabot.yml. The schema mirrors GitHub's v2
// configuration; every optional scalar is a pointer so Parse can tell "absent"
// from "set to the zero value" (which the audit relies on to flag missing
// best-practice settings).
type Config struct {
	Version              *int                `yaml:"version,omitempty"`
	Updates              []Update            `yaml:"updates,omitempty"`
	Registries           map[string]Registry `yaml:"registries,omitempty"`
	EnableBetaEcosystems *bool               `yaml:"enable-beta-ecosystems,omitempty"`
}

// Update is one entry in the top-level `updates` list: a single package
// ecosystem in a directory, plus how Dependabot should manage its PRs.
type Update struct {
	PackageEcosystem              string           `yaml:"package-ecosystem,omitempty"`
	Directory                     string           `yaml:"directory,omitempty"`
	Directories                   []string         `yaml:"directories,omitempty"`
	Schedule                      *Schedule        `yaml:"schedule,omitempty"`
	Allow                         []yaml.Node      `yaml:"allow,omitempty"`
	Assignees                     []string         `yaml:"assignees,omitempty"`
	CommitMessage                 *CommitMessage   `yaml:"commit-message,omitempty"`
	Cooldown                      *Cooldown        `yaml:"cooldown,omitempty"`
	Groups                        map[string]Group `yaml:"groups,omitempty"`
	Ignore                        []yaml.Node      `yaml:"ignore,omitempty"`
	InsecureExternalCodeExecution string           `yaml:"insecure-external-code-execution,omitempty"`
	Labels                        []string         `yaml:"labels,omitempty"`
	Milestone                     *int             `yaml:"milestone,omitempty"`
	OpenPullRequestsLimit         *int             `yaml:"open-pull-requests-limit,omitempty"`
	PullRequestBranchName         *yaml.Node       `yaml:"pull-request-branch-name,omitempty"`
	RebaseStrategy                string           `yaml:"rebase-strategy,omitempty"`
	Registries                    []string         `yaml:"registries,omitempty"`
	Reviewers                     []string         `yaml:"reviewers,omitempty"`
	TargetBranch                  string           `yaml:"target-branch,omitempty"`
	Vendor                        *bool            `yaml:"vendor,omitempty"`
	VersioningStrategy            string           `yaml:"versioning-strategy,omitempty"`
}

// Schedule is an update's `schedule` block.
type Schedule struct {
	Interval string `yaml:"interval,omitempty"` // daily | weekly | monthly | quarterly | semiannually | yearly
	Day      string `yaml:"day,omitempty"`
	Time     string `yaml:"time,omitempty"`
	Timezone string `yaml:"timezone,omitempty"`
}

// CommitMessage is an update's `commit-message` block.
type CommitMessage struct {
	Prefix            string `yaml:"prefix,omitempty"`
	PrefixDevelopment string `yaml:"prefix-development,omitempty"`
	Include           string `yaml:"include,omitempty"`
}

// Cooldown is an update's `cooldown` block: how long to wait after a release
// before opening an update PR for it (a minimum release age).
type Cooldown struct {
	DefaultDays     *int     `yaml:"default-days,omitempty"`
	SemverMajorDays *int     `yaml:"semver-major-days,omitempty"`
	SemverMinorDays *int     `yaml:"semver-minor-days,omitempty"`
	SemverPatchDays *int     `yaml:"semver-patch-days,omitempty"`
	Include         []string `yaml:"include,omitempty"`
	Exclude         []string `yaml:"exclude,omitempty"`
}

// Group is one entry under an update's `groups` block: a named bundle of
// dependencies updated together in a single PR.
type Group struct {
	AppliesTo       string   `yaml:"applies-to,omitempty"` // version-updates | security-updates
	DependencyType  string   `yaml:"dependency-type,omitempty"`
	Patterns        []string `yaml:"patterns,omitempty"`
	ExcludePatterns []string `yaml:"exclude-patterns,omitempty"`
	UpdateTypes     []string `yaml:"update-types,omitempty"`
}

// Registry is one entry under the top-level `registries` block. Its fields vary
// by type; the common ones are modeled so a strict parse accepts real configs.
type Registry struct {
	Type                 string `yaml:"type,omitempty"`
	URL                  string `yaml:"url,omitempty"`
	Username             string `yaml:"username,omitempty"`
	Password             string `yaml:"password,omitempty"`
	Key                  string `yaml:"key,omitempty"`
	Token                string `yaml:"token,omitempty"`
	ReplacesBase         *bool  `yaml:"replaces-base,omitempty"`
	Organization         string `yaml:"organization,omitempty"`
	Repo                 string `yaml:"repo,omitempty"`
	AuthKey              string `yaml:"auth-key,omitempty"`
	PublicKeyFingerprint string `yaml:"public-key-fingerprint,omitempty"`
}

// validIntervals is the closed set GitHub accepts for schedule.interval.
var validIntervals = []string{"daily", "weekly", "monthly", "quarterly", "semiannually", "yearly"}

// Parse decodes a .github/dependabot.yml document. Unknown keys are rejected so
// a typo (which Dependabot would silently ignore, leaving an ecosystem
// unmanaged) surfaces as an error. The document must be a v2 config with at
// least one update entry, and each update must name a valid ecosystem, a
// directory, and a schedule interval.
func Parse(data []byte) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("dependabot config is empty")
		}
		return Config{}, fmt.Errorf("parse dependabot config: %w", err)
	}
	if cfg.Version == nil {
		return Config{}, fmt.Errorf("dependabot config is missing the required 'version' key")
	}
	if *cfg.Version != 2 {
		return Config{}, fmt.Errorf("unsupported dependabot version %d (shuck audits version 2)", *cfg.Version)
	}
	if len(cfg.Updates) == 0 {
		return Config{}, fmt.Errorf("dependabot config declares no updates")
	}
	for i, u := range cfg.Updates {
		if u.PackageEcosystem == "" {
			return Config{}, fmt.Errorf("updates[%d] is missing package-ecosystem", i)
		}
		if !slices.Contains(KnownEcosystems, u.PackageEcosystem) {
			return Config{}, fmt.Errorf("updates[%d] has unknown package-ecosystem %q", i, u.PackageEcosystem)
		}
		if u.Directory == "" && len(u.Directories) == 0 {
			return Config{}, fmt.Errorf("updates[%d] (%s) is missing directory or directories", i, u.PackageEcosystem)
		}
		if u.Schedule == nil || u.Schedule.Interval == "" {
			return Config{}, fmt.Errorf("updates[%d] (%s) is missing schedule.interval", i, u.PackageEcosystem)
		}
		if !slices.Contains(validIntervals, u.Schedule.Interval) {
			return Config{}, fmt.Errorf("updates[%d] (%s) has invalid schedule.interval %q (want: %s)",
				i, u.PackageEcosystem, u.Schedule.Interval, strings.Join(validIntervals, "|"))
		}
	}
	return cfg, nil
}

// dirs returns the normalized directory set an update entry covers (Directory
// plus Directories), each with a leading slash and no trailing slash.
func (u Update) dirs() []string {
	var out []string
	if u.Directory != "" {
		out = append(out, normalizeDir(u.Directory))
	}
	for _, d := range u.Directories {
		out = append(out, normalizeDir(d))
	}
	return out
}

// hasAssignment reports whether the entry names anyone to shepherd its PRs,
// via assignees or the (deprecated) reviewers key.
func (u Update) hasAssignment() bool {
	return len(u.Assignees) > 0 || len(u.Reviewers) > 0
}
