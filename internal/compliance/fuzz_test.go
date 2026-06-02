package compliance

import (
	"io"
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

// FuzzComplianceParse exercises the strict YAML config parser with arbitrary
// bytes, then drives every accepted config through Evaluate and the renderers.
// Nothing in that pipeline may panic; an accepted config must declare at least
// one section; and every check Evaluate emits must be well-formed (non-empty
// setting, a known status).
func FuzzComplianceParse(f *testing.F) {
	f.Add([]byte(``))
	f.Add([]byte(`repository:
  visibility: public
  delete_branch_on_merge: true
security:
  secret_scanning: true
branch_protection:
  main:
    required_approving_review_count: 1
    required_status_checks: [test, lint]
`))
	f.Add([]byte(`repository:
  visibility: bogus
`))
	f.Add([]byte(`actions:
  enabled: true
  allowed_actions: all
  default_workflow_permissions: read
  can_approve_pull_request_reviews: false
  fork_pr_contributor_approval: first_time_contributors
`))
	f.Add([]byte(`actions:
  allowed_actions: everything
`))
	f.Add([]byte(`repository:
  squash_merge_commit_title: PR_TITLE
  merge_commit_message: BLANK
`))
	f.Add([]byte(`unknown_key: true`))
	f.Add([]byte(`branch_protection:
  main: ~
`))
	f.Add([]byte("repository: []\n"))
	f.Add([]byte("&a [*a]"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Cap the input so the fuzzer spends its budget on structure, not on
		// feeding the YAML parser megabytes of noise.
		if len(data) > 8<<10 {
			return
		}

		cfg, err := Parse(data)
		if err != nil {
			return
		}

		// Parse rejects configs that declare nothing.
		if cfg.Repository == nil && cfg.Security == nil && cfg.Actions == nil && len(cfg.BranchProtection) == 0 {
			t.Fatalf("Parse accepted an empty config: %q", data)
		}
		// Parse rejects invalid visibility values.
		if cfg.Repository != nil && cfg.Repository.Visibility != nil {
			switch *cfg.Repository.Visibility {
			case "public", "private", "internal":
			default:
				t.Fatalf("Parse accepted invalid visibility %q", *cfg.Repository.Visibility)
			}
		}
		// Parse rejects invalid closed-vocabulary actions values.
		if cfg.Actions != nil {
			if p := cfg.Actions.DefaultWorkflowPermissions; p != nil && *p != "read" && *p != "write" {
				t.Fatalf("Parse accepted invalid default_workflow_permissions %q", *p)
			}
			if a := cfg.Actions.AllowedActions; a != nil && *a != "all" && *a != "local_only" && *a != "selected" {
				t.Fatalf("Parse accepted invalid allowed_actions %q", *a)
			}
		}

		// Every accepted config must evaluate and render without panicking,
		// whatever the live settings look like.
		for _, actual := range []Actual{
			{},
			{
				Settings: model.RepoSettings{Visibility: "public", SecuritySource: model.SettingsSource{Status: model.StatusOK}},
				Branches: map[string]Branch{"main": {Protection: model.BranchProtection{Protected: true}, Source: model.SettingsSource{Status: model.StatusOK}}},
			},
		} {
			rep := Evaluate("owner", "repo", "fuzz", cfg, actual)
			for _, c := range rep.Checks {
				if c.Setting == "" || c.Category == "" {
					t.Fatalf("Evaluate emitted a check with empty category/setting: %+v", c)
				}
				switch c.Status {
				case model.CompliancePass, model.ComplianceFail, model.ComplianceSkipped, model.ComplianceError:
				default:
					t.Fatalf("Evaluate emitted an unknown status %q: %+v", c.Status, c)
				}
			}
			Render(io.Discard, rep)
			if err := EncodeJSON(io.Discard, rep); err != nil {
				t.Fatalf("EncodeJSON failed: %v", err)
			}
		}
	})
}
