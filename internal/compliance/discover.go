package compliance

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/justanotherspy/shuck/internal/model"
)

// Discovery is the result of building a compliance config from a repository's
// live settings (`shuck compliance discover`). Owner, Repo, and Path identify
// what was discovered and where the config lives; the caller fills them in
// since this package does no I/O.
type Discovery struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Path  string `json:"path"`

	// Data is the config file contents reflecting the live settings. When the
	// config was already up to date it is the existing contents, unchanged.
	Data []byte `json:"-"`

	// Created is true when there was no existing config: Data is a full
	// snapshot of every readable live setting.
	Created bool `json:"created"`

	// Changed is true when Data differs from the existing config (always true
	// for Created).
	Changed bool `json:"changed"`

	// Changes lists the declared settings whose values were synced to the live
	// values (update mode only).
	Changes []Change `json:"changes"`

	// Notes explains settings that could not be discovered (unreadable with
	// this token) or were deliberately left alone.
	Notes []string `json:"notes"`
}

// Change records one declared setting whose value was synced to the live value.
type Change struct {
	Category string `json:"category"` // repository | security | branch_protection
	Setting  string `json:"setting"`  // e.g. allow_merge_commit, main.enforce_admins
	From     string `json:"from"`     // the previously declared value
	To       string `json:"to"`       // the live value now declared

	path  []string // yaml key path to the value node
	value any      // the replacement value, in its natural Go type
}

// Discover builds the compliance config contents that snapshot the live
// settings in actual. When existing is nil a complete config is generated from
// every readable setting. Otherwise the existing config is the template: only
// the keys it already declares are kept, and each declared value that drifted
// from a readable live value is updated in place — comments and key order are
// preserved. Settings the token cannot read are omitted from a new config and
// left untouched in an existing one.
func Discover(existing []byte, actual Actual) (Discovery, error) {
	if existing == nil {
		cfg, notes := FromActual(actual)
		data, err := marshalConfig(cfg)
		if err != nil {
			return Discovery{}, err
		}
		return Discovery{Data: data, Created: true, Changed: true, Notes: notes}, nil
	}

	cfg, err := Parse(existing)
	if err != nil {
		return Discovery{}, err
	}

	changes, notes := diffConfig(cfg, actual)
	if len(changes) == 0 {
		return Discovery{Data: existing, Notes: notes}, nil
	}

	data, err := patchYAML(existing, changes)
	if err != nil {
		return Discovery{}, err
	}
	return Discovery{Data: data, Changed: true, Changes: changes, Notes: notes}, nil
}

// FromActual builds a complete Config snapshotting every readable live setting
// in actual, plus notes for the settings that had to be omitted (unreadable, or
// a branch with no protection rule to snapshot).
func FromActual(actual Actual) (cfg Config, notes []string) {
	s := actual.Settings

	cfg = Config{Repository: &RepositoryConfig{
		Visibility:               new(s.Visibility),
		DefaultBranch:            new(s.DefaultBranch),
		HasIssues:                new(s.HasIssues),
		HasWiki:                  new(s.HasWiki),
		HasProjects:              new(s.HasProjects),
		HasDiscussions:           new(s.HasDiscussions),
		WebCommitSignoffRequired: new(s.WebCommitSignoffRequired),
		Archived:                 new(s.Archived),
	}}
	// The merge-policy fields are invisible to fine-grained / app tokens; only
	// snapshot them when they were actually readable.
	if s.MergeSettingsSource.Status == model.StatusOK {
		cfg.Repository.AllowMergeCommit = new(s.AllowMergeCommit)
		cfg.Repository.AllowSquashMerge = new(s.AllowSquashMerge)
		cfg.Repository.AllowRebaseMerge = new(s.AllowRebaseMerge)
		cfg.Repository.AllowAutoMerge = new(s.AllowAutoMerge)
		cfg.Repository.AllowUpdateBranch = new(s.AllowUpdateBranch)
		cfg.Repository.DeleteBranchOnMerge = new(s.DeleteBranchOnMerge)
	} else {
		notes = append(notes, "merge settings omitted: "+sourceReason(s.MergeSettingsSource))
	}

	var sec SecurityConfig
	hasSecurity := false
	if s.SecuritySource.Status == model.StatusOK {
		sec.SecretScanning = new(s.SecretScanning == "enabled")
		sec.SecretScanningPushProtection = new(s.SecretScanningPushProtection == "enabled")
		sec.DependabotSecurityUpdates = new(s.DependabotSecurityUpdates == "enabled")
		hasSecurity = true
	} else {
		notes = append(notes, "security settings omitted: "+sourceReason(s.SecuritySource))
	}
	if actual.VulnSource.Status == model.StatusOK {
		sec.VulnerabilityAlerts = new(actual.VulnAlerts)
		hasSecurity = true
	} else {
		notes = append(notes, "vulnerability_alerts omitted: "+sourceReason(actual.VulnSource))
	}
	if hasSecurity {
		cfg.Security = &sec
	}

	for _, name := range sortedActualBranches(actual.Branches) {
		br := actual.Branches[name]
		if br.Source.Status != model.StatusOK {
			notes = append(notes, fmt.Sprintf("branch %s protection omitted: %s", name, sourceReason(br.Source)))
			continue
		}
		if !br.Protection.Protected {
			notes = append(notes, fmt.Sprintf("branch %s has no protection rule — nothing to declare", name))
			continue
		}
		if cfg.BranchProtection == nil {
			cfg.BranchProtection = make(map[string]*BranchConfig)
		}
		cfg.BranchProtection[name] = branchConfigFrom(br.Protection)
	}

	return cfg, notes
}

// branchConfigFrom snapshots one protected branch's live rule. Review settings
// are declared only when pull-request reviews are required at all, and status
// checks only when at least one is required, so the generated config does not
// assert the zero values of absent sub-rules.
func branchConfigFrom(bp model.BranchProtection) *BranchConfig {
	bc := &BranchConfig{
		RequireLinearHistory:          new(bp.RequireLinearHistory),
		AllowForcePushes:              new(bp.AllowForcePushes),
		AllowDeletions:                new(bp.AllowDeletions),
		RequireConversationResolution: new(bp.RequireConversationResolution),
		RequiredSignatures:            new(bp.RequiredSignatures),
	}
	// enforce_admins is a classic-protection concept; for a branch protected only
	// by rulesets its value is not knowable, so it is not declared.
	if !bp.ViaRulesetsOnly {
		bc.EnforceAdmins = new(bp.EnforceAdmins)
	}
	if bp.RequiredPullRequestReviews {
		bc.RequiredApprovingReviewCount = new(bp.RequiredApprovingReviewCount)
		bc.DismissStaleReviews = new(bp.DismissStaleReviews)
		bc.RequireCodeOwnerReviews = new(bp.RequireCodeOwnerReviews)
		bc.RequireLastPushApproval = new(bp.RequireLastPushApproval)
	}
	if len(bp.RequiredStatusChecks) > 0 {
		checks := append([]string(nil), bp.RequiredStatusChecks...)
		sort.Strings(checks)
		bc.RequiredStatusChecks = &checks
		bc.StrictStatusChecks = new(bp.StrictStatusChecks)
	}
	return bc
}

// diffConfig finds every key cfg declares whose readable live value differs,
// returning the changes (with the yaml path + replacement value needed to patch
// the file) and notes for declared keys that were left alone.
func diffConfig(cfg Config, actual Actual) (changes []Change, notes []string) {
	d := &differ{}

	if r := cfg.Repository; r != nil {
		s := actual.Settings
		diffField(d, "repository", "visibility", r.Visibility, s.Visibility)
		diffField(d, "repository", "default_branch", r.DefaultBranch, s.DefaultBranch)
		// Merge-policy fields can only be synced when they were readable; syncing
		// an invisible (all-false) group would corrupt the declared intent.
		if s.MergeSettingsSource.Status == model.StatusOK {
			diffField(d, "repository", "allow_merge_commit", r.AllowMergeCommit, s.AllowMergeCommit)
			diffField(d, "repository", "allow_squash_merge", r.AllowSquashMerge, s.AllowSquashMerge)
			diffField(d, "repository", "allow_rebase_merge", r.AllowRebaseMerge, s.AllowRebaseMerge)
			diffField(d, "repository", "allow_auto_merge", r.AllowAutoMerge, s.AllowAutoMerge)
			diffField(d, "repository", "allow_update_branch", r.AllowUpdateBranch, s.AllowUpdateBranch)
			diffField(d, "repository", "delete_branch_on_merge", r.DeleteBranchOnMerge, s.DeleteBranchOnMerge)
		} else if anyDeclared(r.AllowMergeCommit, r.AllowSquashMerge, r.AllowRebaseMerge,
			r.AllowAutoMerge, r.AllowUpdateBranch, r.DeleteBranchOnMerge) {
			d.notes = append(d.notes, "merge settings left unchanged: "+sourceReason(s.MergeSettingsSource))
		}
		diffField(d, "repository", "has_issues", r.HasIssues, s.HasIssues)
		diffField(d, "repository", "has_wiki", r.HasWiki, s.HasWiki)
		diffField(d, "repository", "has_projects", r.HasProjects, s.HasProjects)
		diffField(d, "repository", "has_discussions", r.HasDiscussions, s.HasDiscussions)
		diffField(d, "repository", "web_commit_signoff_required", r.WebCommitSignoffRequired, s.WebCommitSignoffRequired)
		diffField(d, "repository", "archived", r.Archived, s.Archived)
	}

	if sec := cfg.Security; sec != nil {
		s := actual.Settings
		if s.SecuritySource.Status == model.StatusOK {
			diffField(d, "security", "secret_scanning", sec.SecretScanning, s.SecretScanning == "enabled")
			diffField(d, "security", "secret_scanning_push_protection", sec.SecretScanningPushProtection, s.SecretScanningPushProtection == "enabled")
			diffField(d, "security", "dependabot_security_updates", sec.DependabotSecurityUpdates, s.DependabotSecurityUpdates == "enabled")
		} else if anyDeclared(sec.SecretScanning, sec.SecretScanningPushProtection, sec.DependabotSecurityUpdates) {
			d.notes = append(d.notes, "security settings left unchanged: "+sourceReason(s.SecuritySource))
		}
		if sec.VulnerabilityAlerts != nil {
			if actual.VulnSource.Status == model.StatusOK {
				diffField(d, "security", "vulnerability_alerts", sec.VulnerabilityAlerts, actual.VulnAlerts)
			} else {
				d.notes = append(d.notes, "vulnerability_alerts left unchanged: "+sourceReason(actual.VulnSource))
			}
		}
	}

	for _, name := range sortedBranches(cfg.BranchProtection) {
		bc := cfg.BranchProtection[name]
		if bc == nil {
			continue
		}
		diffBranch(d, name, bc, actual.Branches[name])
	}

	return d.changes, d.notes
}

// diffBranch diffs one declared branch's protections against its live rule.
func diffBranch(d *differ, name string, bc *BranchConfig, br Branch) {
	if br.Source.Status != model.StatusOK {
		d.notes = append(d.notes, fmt.Sprintf("branch %s protections left unchanged: %s", name, sourceReason(br.Source)))
		return
	}
	bp := br.Protection
	if !bp.Protected {
		d.notes = append(d.notes, fmt.Sprintf("branch %s has no protection rule — declared protections left unchanged", name))
		return
	}

	diffBranchField(d, name, "required_approving_review_count", bc.RequiredApprovingReviewCount, bp.RequiredApprovingReviewCount)
	diffBranchField(d, name, "dismiss_stale_reviews", bc.DismissStaleReviews, bp.DismissStaleReviews)
	diffBranchField(d, name, "require_code_owner_reviews", bc.RequireCodeOwnerReviews, bp.RequireCodeOwnerReviews)
	diffBranchField(d, name, "require_last_push_approval", bc.RequireLastPushApproval, bp.RequireLastPushApproval)
	diffBranchField(d, name, "strict_status_checks", bc.StrictStatusChecks, bp.StrictStatusChecks)
	// enforce_admins has no readable ruleset equivalent (bypass actors are not
	// visible via the rules API), so it is never synced from ruleset-only
	// protection — the declared intent stays.
	if bc.EnforceAdmins != nil && bp.ViaRulesetsOnly {
		d.notes = append(d.notes, fmt.Sprintf("branch %s enforce_admins left unchanged: branch is protected by rulesets (admin bypass is not visible via the rules API)", name))
	} else {
		diffBranchField(d, name, "enforce_admins", bc.EnforceAdmins, bp.EnforceAdmins)
	}
	diffBranchField(d, name, "required_linear_history", bc.RequireLinearHistory, bp.RequireLinearHistory)
	diffBranchField(d, name, "allow_force_pushes", bc.AllowForcePushes, bp.AllowForcePushes)
	diffBranchField(d, name, "allow_deletions", bc.AllowDeletions, bp.AllowDeletions)
	diffBranchField(d, name, "required_conversation_resolution", bc.RequireConversationResolution, bp.RequireConversationResolution)
	diffBranchField(d, name, "required_signatures", bc.RequiredSignatures, bp.RequiredSignatures)

	if bc.RequiredStatusChecks != nil && !equalSets(*bc.RequiredStatusChecks, bp.RequiredStatusChecks) {
		live := append([]string(nil), bp.RequiredStatusChecks...)
		sort.Strings(live)
		d.changes = append(d.changes, Change{
			Category: "branch_protection", Setting: name + ".required_status_checks",
			From: strings.Join(*bc.RequiredStatusChecks, ", "), To: strings.Join(live, ", "),
			path: []string{"branch_protection", name, "required_status_checks"}, value: live,
		})
	}
}

// differ accumulates the changes and notes of one diffConfig pass.
type differ struct {
	changes []Change
	notes   []string
}

// diffField records a change when a declared repository/security key's live
// value differs. The yaml path is [category, setting].
func diffField[T comparable](d *differ, category, setting string, want *T, got T) {
	if want == nil || *want == got {
		return
	}
	d.changes = append(d.changes, Change{
		Category: category, Setting: setting,
		From: fmt.Sprint(*want), To: fmt.Sprint(got),
		path: []string{category, setting}, value: got,
	})
}

// diffBranchField records a change for one branch-protection key. The display
// setting is "<branch>.<key>" but the yaml path keeps the branch name as a
// single segment (branch names may themselves contain dots).
func diffBranchField[T comparable](d *differ, branch, setting string, want *T, got T) {
	if want == nil || *want == got {
		return
	}
	d.changes = append(d.changes, Change{
		Category: "branch_protection", Setting: branch + "." + setting,
		From: fmt.Sprint(*want), To: fmt.Sprint(got),
		path: []string{"branch_protection", branch, setting}, value: got,
	})
}

// patchYAML applies the changes to the existing config document, preserving its
// comments, key order, and any keys the diff did not touch.
func patchYAML(existing []byte, changes []Change) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, fmt.Errorf("parse compliance config: %w", err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, fmt.Errorf("compliance config is empty")
		}
		root = doc.Content[0]
	}

	for i := range changes {
		node := root
		for _, key := range changes[i].path {
			node = mappingValue(node, key)
			if node == nil {
				return nil, fmt.Errorf("config key %s not found", strings.Join(changes[i].path, "."))
			}
		}
		var repl yaml.Node
		if err := repl.Encode(changes[i].value); err != nil {
			return nil, fmt.Errorf("encode %s: %w", strings.Join(changes[i].path, "."), err)
		}
		// Keep the comments attached to the value being replaced.
		repl.HeadComment, repl.LineComment, repl.FootComment = node.HeadComment, node.LineComment, node.FootComment
		*node = repl
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode compliance config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode compliance config: %w", err)
	}
	return buf.Bytes(), nil
}

// mappingValue returns the value node for key in mapping node m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// generatedHeader is the comment block written at the top of a newly generated
// compliance config.
const generatedHeader = `# The intended GitHub settings for this repository.
#
# Generated by ` + "`shuck compliance discover`" + ` from the repository's live settings.
# This file is the source of truth: ` + "`shuck compliance`" + ` compares the repo's live
# settings against it and reports drift. It is partial by design — only the keys
# declared here are checked — so feel free to trim it down to the settings you
# care about.

`

// marshalConfig renders a Config as a fresh compliance.yml document.
func marshalConfig(cfg Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(generatedHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encode compliance config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode compliance config: %w", err)
	}
	return buf.Bytes(), nil
}

// equalSets compares two string slices as unordered sets.
func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// anyDeclared reports whether any of the pointers is non-nil.
func anyDeclared(ps ...*bool) bool {
	for _, p := range ps {
		if p != nil {
			return true
		}
	}
	return false
}

// sourceReason renders why a settings source could not be read.
func sourceReason(src model.SettingsSource) string {
	if src.Message != "" {
		return src.Message
	}
	return "could not read this setting"
}

func sortedActualBranches(m map[string]Branch) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
