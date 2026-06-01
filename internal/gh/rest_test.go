package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"number": 7,
			"title": "fix things",
			"updated_at": "2026-01-02T03:04:05Z",
			"head": {"sha": "abc123", "ref": "feature"}
		}`))
	}))
	defer srv.Close()

	pr, err := testClient(t, srv).GetPR(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.Owner != "o" || pr.Repo != "r" || pr.Number != 7 {
		t.Errorf("identity = %+v", pr)
	}
	if pr.Title != "fix things" || pr.HeadSHA != "abc123" || pr.HeadBranch != "feature" {
		t.Errorf("fields = %+v", pr)
	}
	if pr.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt not parsed: %v", pr.UpdatedAt)
	}
}

func TestGetPRError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).GetPR(context.Background(), "o", "r", 7); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestDefaultBranchSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/commits/HEAD" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// GetCommitSHA1 reads the plain-text body as the SHA.
		_, _ = w.Write([]byte("deadbeef"))
	}))
	defer srv.Close()

	sha, err := testClient(t, srv).DefaultBranchSHA(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("DefaultBranchSHA: %v", err)
	}
	if sha != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", sha)
	}
}

func TestDefaultBranchSHAError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).DefaultBranchSHA(context.Background(), "o", "r"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFindOpenPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "fork:branch" {
			t.Errorf("head filter = %q", got)
		}
		_, _ = w.Write([]byte(`[{"number": 42}]`))
	}))
	defer srv.Close()

	n, err := testClient(t, srv).FindOpenPR(context.Background(), "o", "r", "fork", "branch")
	if err != nil {
		t.Fatalf("FindOpenPR: %v", err)
	}
	if n != 42 {
		t.Errorf("number = %d, want 42", n)
	}
}

func TestFindOpenPRNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).FindOpenPR(context.Background(), "o", "r", "fork", "branch"); err == nil {
		t.Fatal("expected error when no PR matches")
	}
}

func TestFindOpenPRError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).FindOpenPR(context.Background(), "o", "r", "fork", "branch"); err == nil {
		t.Fatal("expected error on 403")
	}
}

// runsAndJobs serves the two-step runs→jobs sequence ListJobs and RunReport
// walk, returning one failed and one running job. The runs listing is paginated
// to exercise listRuns' Link-header loop.
func runsAndJobsHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/actions/runs":
			if r.URL.Query().Get("page") == "" {
				// First page links to a second to exercise pagination.
				w.Header().Set("Link", `<`+linkBase(r)+`?page=2>; rel="next"`)
				_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":100,"path":".github/workflows/ci.yml"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
		case "/repos/o/r/actions/runs/100/jobs":
			_, _ = w.Write([]byte(`{"total_count":2,"jobs":[
				{"id":1,"name":"build","status":"completed","conclusion":"failure","run_attempt":1,
				 "steps":[{"number":1,"name":"test","status":"completed","conclusion":"failure"}]},
				{"id":2,"name":"deploy","status":"in_progress"}
			]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func linkBase(r *http.Request) string {
	return "http://" + r.Host + r.URL.Path
}

func TestListJobs(t *testing.T) {
	srv := httptest.NewServer(runsAndJobsHandler(t))
	defer srv.Close()

	failed, cancelled, running, err := testClient(t, srv).ListJobs(context.Background(), "o", "r", "sha")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(failed) != 1 || failed[0].Name != "build" || failed[0].RunID != 100 {
		t.Errorf("failed = %+v", failed)
	}
	if len(cancelled) != 0 {
		t.Errorf("cancelled = %+v, want none", cancelled)
	}
	if len(running) != 1 || running[0].Name != "deploy" {
		t.Errorf("running = %+v", running)
	}
}

func TestListJobsRunsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, _, _, err := testClient(t, srv).ListJobs(context.Background(), "o", "r", "sha"); err == nil {
		t.Fatal("expected error")
	}
}

func TestListJobsJobsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/runs" {
			_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":100}]}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, _, _, err := testClient(t, srv).ListJobs(context.Background(), "o", "r", "sha"); err == nil {
		t.Fatal("expected error from job listing")
	}
}

func TestRunReportAllJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/actions/runs/100":
			_, _ = w.Write([]byte(`{"id":100,"display_title":"CI","head_sha":"sha","head_branch":"main","name":"build","path":".github/workflows/ci.yml"}`))
		case "/repos/o/r/actions/runs/100/jobs":
			_, _ = w.Write([]byte(`{"total_count":1,"jobs":[
				{"id":1,"name":"build","status":"completed","conclusion":"failure"}
			]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	info, failed, _, _, err := testClient(t, srv).RunReport(context.Background(), "o", "r", 100, 0)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if info.Title != "CI" || info.WorkflowName != "build" || info.RunID != 100 {
		t.Errorf("info = %+v", info)
	}
	if len(failed) != 1 || failed[0].Name != "build" {
		t.Errorf("failed = %+v", failed)
	}
}

func TestRunReportSingleJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/actions/runs/100":
			_, _ = w.Write([]byte(`{"id":100,"display_title":"CI"}`))
		case "/repos/o/r/actions/jobs/5":
			_, _ = w.Write([]byte(`{"id":5,"name":"e2e","status":"completed","conclusion":"failure"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	info, failed, _, _, err := testClient(t, srv).RunReport(context.Background(), "o", "r", 100, 5)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if info.JobID != 5 {
		t.Errorf("JobID = %d, want 5", info.JobID)
	}
	if len(failed) != 1 || failed[0].Name != "e2e" {
		t.Errorf("failed = %+v", failed)
	}
}

func TestRunReportRunError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, _, _, _, err := testClient(t, srv).RunReport(context.Background(), "o", "r", 100, 0); err == nil {
		t.Fatal("expected error")
	}
}

func TestRunReportJobError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/runs/100" {
			_, _ = w.Write([]byte(`{"id":100}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, _, _, _, err := testClient(t, srv).RunReport(context.Background(), "o", "r", 100, 5); err == nil {
		t.Fatal("expected error fetching single job")
	}
}

func TestJobLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/jobs/9/logs" {
			http.Redirect(w, r, linkBase(r)+"/blob", http.StatusFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/blob") {
			_, _ = w.Write([]byte("the log body"))
			return
		}
		t.Errorf("unexpected path %q", r.URL.Path)
	}))
	defer srv.Close()

	log, err := testClient(t, srv).JobLog(context.Background(), "o", "r", 9)
	if err != nil {
		t.Fatalf("JobLog: %v", err)
	}
	if log != "the log body" {
		t.Errorf("log = %q", log)
	}
}

func TestJobLogURLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).JobLog(context.Background(), "o", "r", 9); err == nil {
		t.Fatal("expected error when the log URL cannot be resolved")
	}
}

func TestJobLogRedirectTargetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/jobs/9/logs" {
			http.Redirect(w, r, linkBase(r)+"/blob", http.StatusFound)
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).JobLog(context.Background(), "o", "r", 9); err == nil {
		t.Fatal("expected error when the redirect target returns non-200")
	}
}

func TestOtherChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/sha/check-runs":
			// One failing external check, one github-actions (skipped), one
			// pending (skipped), one passing (skipped).
			_, _ = w.Write([]byte(`{"total_count":4,"check_runs":[
				{"name":"sonar","status":"completed","conclusion":"failure","details_url":"https://sonar","app":{"slug":"sonarcloud"}},
				{"name":"ci","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}},
				{"name":"pending","status":"in_progress","app":{"slug":"other"}},
				{"name":"ok","status":"completed","conclusion":"success","app":{"slug":"other"}}
			]}`))
		case "/repos/o/r/commits/sha/status":
			_, _ = w.Write([]byte(`{"state":"failure","statuses":[
				{"context":"legacy","state":"error","target_url":"https://legacy"},
				{"context":"green","state":"success"}
			]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	checks, err := testClient(t, srv).OtherChecks(context.Background(), "o", "r", "sha")
	if err != nil {
		t.Fatalf("OtherChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want 2", checks)
	}
	if checks[0].Name != "sonar" || checks[0].URL != "https://sonar" {
		t.Errorf("check[0] = %+v", checks[0])
	}
	if checks[1].Name != "legacy" || checks[1].Conclusion != "error" {
		t.Errorf("check[1] = %+v", checks[1])
	}
}

func TestOtherChecksCheckRunsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).OtherChecks(context.Background(), "o", "r", "sha"); err == nil {
		t.Fatal("expected error")
	}
}

func TestOtherChecksStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).OtherChecks(context.Background(), "o", "r", "sha"); err == nil {
		t.Fatal("expected error from combined status")
	}
}

func TestNew(t *testing.T) {
	authed := New("tok")
	if authed.gh == nil || authed.http == nil || authed.token != "tok" {
		t.Errorf("authed client malformed: %+v", authed)
	}
	if authed.graphqlURL != graphQLEndpoint || authed.registryURL != registryHost {
		t.Errorf("default endpoints not set: %q %q", authed.graphqlURL, authed.registryURL)
	}
	anon := New("")
	if anon.gh == nil || anon.token != "" {
		t.Errorf("anon client malformed: %+v", anon)
	}
}
