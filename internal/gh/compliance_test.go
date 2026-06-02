package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v88/github"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestRepoSettingsMergeFieldsPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"visibility": "public",
			"default_branch": "main",
			"allow_merge_commit": false,
			"allow_squash_merge": true,
			"delete_branch_on_merge": true,
			"has_wiki": false
		}`))
	}))
	defer srv.Close()

	s, err := testClient(t, srv).RepoSettings(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("RepoSettings: %v", err)
	}
	if s.MergeSettingsSource.Status != model.StatusOK {
		t.Errorf("merge settings should be readable: %+v", s.MergeSettingsSource)
	}
	if !s.AllowSquashMerge || s.AllowMergeCommit || !s.DeleteBranchOnMerge {
		t.Errorf("merge settings wrong: %+v", s)
	}
}

func TestRepoSettingsMergeFieldsAbsent(t *testing.T) {
	// A fine-grained PAT / app installation token: GitHub omits the merge-policy
	// fields entirely, even with admin access.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"visibility": "public",
			"default_branch": "main",
			"has_wiki": false,
			"security_and_analysis": {"secret_scanning": {"status": "enabled"}}
		}`))
	}))
	defer srv.Close()

	s, err := testClient(t, srv).RepoSettings(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("RepoSettings: %v", err)
	}
	if s.MergeSettingsSource.Status != model.StatusForbidden {
		t.Errorf("absent merge settings must be forbidden, not silently false: %+v", s.MergeSettingsSource)
	}
	if !strings.Contains(s.MergeSettingsSource.Message, "classic PAT") {
		t.Errorf("message should point at the token type: %q", s.MergeSettingsSource.Message)
	}
	// The rest of the settings remain readable.
	if s.Visibility != "public" || s.SecuritySource.Status != model.StatusOK {
		t.Errorf("non-merge settings should still be read: %+v", s)
	}
}

func TestActionsSettingsAllReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled": true, "allowed_actions": "selected", "sha_pinning_required": true}`))
		case "/repos/o/r/actions/permissions/workflow":
			_, _ = w.Write([]byte(`{"default_workflow_permissions": "read", "can_approve_pull_request_reviews": false}`))
		case "/repos/o/r/actions/permissions/fork-pr-contributor-approval":
			_, _ = w.Write([]byte(`{"approval_policy": "first_time_contributors"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	s := testClient(t, srv).ActionsSettings(context.Background(), "o", "r")
	if s.PermissionsSource.Status != model.StatusOK || s.WorkflowPermissionsSource.Status != model.StatusOK || s.ForkPRApprovalSource.Status != model.StatusOK {
		t.Fatalf("all sources should be ok: %+v", s)
	}
	if !s.Enabled || s.AllowedActions != "selected" || !s.SHAPinningRequired {
		t.Errorf("permissions not mapped: %+v", s)
	}
	if s.DefaultWorkflowPermissions != "read" || s.CanApprovePullRequestReviews {
		t.Errorf("workflow permissions not mapped: %+v", s)
	}
	if s.ForkPRContributorApproval != "first_time_contributors" {
		t.Errorf("fork approval policy not mapped: %+v", s)
	}
}

func TestActionsSettingsDegradesPerGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/actions/permissions":
			_, _ = w.Write([]byte(`{"enabled": true, "allowed_actions": "all"}`))
		case "/repos/o/r/actions/permissions/workflow":
			http.Error(w, `{"message":"Must have admin rights"}`, http.StatusForbidden)
		case "/repos/o/r/actions/permissions/fork-pr-contributor-approval":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	s := testClient(t, srv).ActionsSettings(context.Background(), "o", "r")
	if s.PermissionsSource.Status != model.StatusOK || !s.Enabled || s.AllowedActions != "all" {
		t.Errorf("readable permissions group should map: %+v", s)
	}
	if s.WorkflowPermissionsSource.Status != model.StatusForbidden {
		t.Errorf("403 should be a forbidden source, got %+v", s.WorkflowPermissionsSource)
	}
	if s.ForkPRApprovalSource.Status != model.StatusDisabled {
		t.Errorf("404 should be a disabled source, got %+v", s.ForkPRApprovalSource)
	}
	// Unreadable groups keep their zero values: the evaluator skips them.
	if s.DefaultWorkflowPermissions != "" || s.CanApprovePullRequestReviews {
		t.Errorf("unreadable group must stay zero: %+v", s)
	}
}

// branchRulesJSON is the /rules/branches/<branch> response of a branch governed
// by a squash-only ruleset (modeled on a real GitHub response).
const branchRulesJSON = `[
	{"type": "deletion", "ruleset_id": 1},
	{"type": "non_fast_forward", "ruleset_id": 1},
	{"type": "required_signatures", "ruleset_id": 1},
	{"type": "pull_request", "ruleset_id": 1, "parameters": {
		"required_approving_review_count": 1,
		"dismiss_stale_reviews_on_push": true,
		"require_code_owner_review": false,
		"require_last_push_approval": false,
		"required_review_thread_resolution": true,
		"allowed_merge_methods": ["squash"]
	}},
	{"type": "required_status_checks", "ruleset_id": 1, "parameters": {
		"strict_required_status_checks_policy": false,
		"required_status_checks": [{"context": "Test"}, {"context": "Lint"}]
	}}
]`

func TestBranchProtectionSettingsRulesetsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection":
			// No classic protection rule.
			http.Error(w, `{"message":"Branch not protected"}`, http.StatusNotFound)
		case "/repos/o/r/rules/branches/main":
			_, _ = w.Write([]byte(branchRulesJSON))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	bp, src := testClient(t, srv).BranchProtectionSettings(context.Background(), "o", "r", "main")
	if src.Status != model.StatusOK {
		t.Fatalf("source = %+v, want ok", src)
	}
	if !bp.Protected || !bp.ViaRulesetsOnly {
		t.Fatalf("branch should be protected via rulesets: %+v", bp)
	}
	if !bp.RequiredPullRequestReviews || bp.RequiredApprovingReviewCount != 1 {
		t.Errorf("pull_request rule not mapped: %+v", bp)
	}
	if !bp.DismissStaleReviews || !bp.RequireConversationResolution {
		t.Errorf("pull_request params not mapped: %+v", bp)
	}
	if bp.AllowForcePushes || bp.AllowDeletions {
		t.Errorf("non_fast_forward/deletion rules should block: %+v", bp)
	}
	if !bp.RequiredSignatures || bp.RequireLinearHistory {
		t.Errorf("signature/linear-history rules wrong: %+v", bp)
	}
	if len(bp.RequiredStatusChecks) != 2 {
		t.Errorf("status checks not mapped: %v", bp.RequiredStatusChecks)
	}
}

func TestBranchProtectionSettingsClassicOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection":
			_, _ = w.Write([]byte(`{
				"required_pull_request_reviews": {"required_approving_review_count": 2, "dismiss_stale_reviews": true},
				"enforce_admins": {"enabled": true},
				"allow_force_pushes": {"enabled": false}
			}`))
		case "/repos/o/r/rules/branches/main":
			_, _ = w.Write([]byte(`[]`)) // no rulesets apply
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	bp, src := testClient(t, srv).BranchProtectionSettings(context.Background(), "o", "r", "main")
	if src.Status != model.StatusOK {
		t.Fatalf("source = %+v, want ok", src)
	}
	if !bp.Protected || bp.ViaRulesetsOnly {
		t.Fatalf("classic protection should not be marked ruleset-only: %+v", bp)
	}
	if bp.RequiredApprovingReviewCount != 2 || !bp.EnforceAdmins {
		t.Errorf("classic protection not mapped: %+v", bp)
	}
}

func TestBranchProtectionSettingsMergesBothSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection":
			// Classic: 1 approval, admins enforced, force pushes allowed.
			_, _ = w.Write([]byte(`{
				"required_pull_request_reviews": {"required_approving_review_count": 1},
				"enforce_admins": {"enabled": true},
				"allow_force_pushes": {"enabled": true},
				"required_status_checks": {"strict": true, "checks": [{"context": "Lint"}]}
			}`))
		case "/repos/o/r/rules/branches/main":
			// Ruleset: 2 approvals, force pushes blocked, an extra status check.
			_, _ = w.Write([]byte(`[
				{"type": "non_fast_forward", "ruleset_id": 1},
				{"type": "pull_request", "ruleset_id": 1, "parameters": {"required_approving_review_count": 2}},
				{"type": "required_status_checks", "ruleset_id": 1, "parameters": {
					"required_status_checks": [{"context": "Lint"}, {"context": "Test"}]
				}}
			]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	bp, src := testClient(t, srv).BranchProtectionSettings(context.Background(), "o", "r", "main")
	if src.Status != model.StatusOK {
		t.Fatalf("source = %+v, want ok", src)
	}
	if bp.ViaRulesetsOnly {
		t.Error("both sources present must not be ruleset-only")
	}
	if bp.RequiredApprovingReviewCount != 2 {
		t.Errorf("stricter review count should win: %d", bp.RequiredApprovingReviewCount)
	}
	if bp.AllowForcePushes {
		t.Error("ruleset blocks force pushes, so they must not be allowed")
	}
	if !bp.EnforceAdmins {
		t.Error("classic enforce_admins must survive the merge")
	}
	if len(bp.RequiredStatusChecks) != 2 || !bp.StrictStatusChecks {
		t.Errorf("status checks should be the union: %+v", bp)
	}
}

func TestBranchProtectionSettingsRulesUnreadable(t *testing.T) {
	// The rules endpoint failing must not change the classic-only result.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection":
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		case "/repos/o/r/rules/branches/main":
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	bp, src := testClient(t, srv).BranchProtectionSettings(context.Background(), "o", "r", "main")
	if src.Status != model.StatusForbidden {
		t.Fatalf("source = %+v, want forbidden", src)
	}
	if bp.Protected {
		t.Errorf("unreadable protection must not be reported as protected: %+v", bp)
	}
}

func TestBranchProtectionSettingsForbiddenClassicReadableRules(t *testing.T) {
	// Classic protection needs admin; rulesets are world-readable on public
	// repos. The ruleset protection should be reported instead of skipping.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection":
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		case "/repos/o/r/rules/branches/main":
			_, _ = w.Write([]byte(`[{"type": "deletion", "ruleset_id": 1}]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	bp, src := testClient(t, srv).BranchProtectionSettings(context.Background(), "o", "r", "main")
	if src.Status != model.StatusOK {
		t.Fatalf("source = %+v, want ok (rules are readable)", src)
	}
	if !bp.Protected || !bp.ViaRulesetsOnly || bp.AllowDeletions {
		t.Errorf("deletion rule not reported: %+v", bp)
	}
}

func TestMapBranchRulesUnprotected(t *testing.T) {
	bp := mapBranchRules("main", &github.BranchRules{})
	if bp.Protected || bp.ViaRulesetsOnly {
		t.Errorf("no rules should mean unprotected: %+v", bp)
	}
	if bp := mapBranchRules("main", nil); bp.Protected {
		t.Errorf("nil rules should mean unprotected: %+v", bp)
	}
}

func TestMapBranchRulesUnmodeledRuleStillProtects(t *testing.T) {
	// A branch governed only by rules shuck does not model (e.g. code scanning)
	// is still "protected": rulesets govern it.
	bp := mapBranchRules("main", &github.BranchRules{
		CodeScanning: []*github.CodeScanningBranchRule{{}},
	})
	if !bp.Protected || !bp.ViaRulesetsOnly {
		t.Errorf("code_scanning rule should mark the branch protected: %+v", bp)
	}
	// Without blocking rules, force pushes and deletions remain allowed.
	if !bp.AllowForcePushes || !bp.AllowDeletions {
		t.Errorf("absent blocking rules should leave actions allowed: %+v", bp)
	}
}

func TestMergeBranchProtectionUnprotectedSides(t *testing.T) {
	classic := model.BranchProtection{Branch: "main", Protected: true, EnforceAdmins: true}
	ruleset := model.BranchProtection{Branch: "main", Protected: true, ViaRulesetsOnly: true, RequiredSignatures: true}

	if got := mergeBranchProtection(classic, model.BranchProtection{}); !got.EnforceAdmins {
		t.Errorf("unprotected ruleset side should return classic: %+v", got)
	}
	if got := mergeBranchProtection(model.BranchProtection{}, ruleset); !got.ViaRulesetsOnly {
		t.Errorf("unprotected classic side should return ruleset: %+v", got)
	}
}

func TestUnionStrings(t *testing.T) {
	got := unionStrings([]string{"a", "b"}, []string{"b", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("union = %v", got)
	}
	if got := unionStrings(nil, nil); len(got) != 0 {
		t.Errorf("empty union = %v", got)
	}
}
