package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justanotherspy/shuck/internal/ingest"
)

// ghAPI serves the minimal GitHub REST surface FetchRun touches, mounted
// under /api/v3/ (go-github's enterprise base URL normalization).
func ghAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("GET /api/v3/repos/o/r/actions/runs/99", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer ghs_inst" {
			t.Errorf("run request auth = %q, want the installation token", auth)
		}
		fmt.Fprint(w, `{"id":99,"name":"CI","head_sha":"abc","head_branch":"main"}`)
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/actions/runs/99/jobs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"total_count":3,"jobs":[
			{"id":1,"run_id":99,"name":"test","status":"completed","conclusion":"failure",
			 "steps":[{"number":1,"name":"go test","status":"completed","conclusion":"failure"}]},
			{"id":2,"run_id":99,"name":"lint","status":"completed","conclusion":"cancelled","steps":[]},
			{"id":3,"run_id":99,"name":"ok","status":"completed","conclusion":"success","steps":[]}
		]}`)
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/actions/jobs/1/logs", func(w http.ResponseWriter, r *http.Request) {
		// The Location must be absolute: gh.JobLog hands it to a fresh
		// request, mirroring GitHub's signed blob-store URL.
		http.Redirect(w, r, srv.URL+"/raw/1", http.StatusFound)
	})
	mux.HandleFunc("GET /raw/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "##[group]Run go test ./...\n##[endgroup]\n##[error]FAIL\n")
	})
	mux.HandleFunc("GET /api/v3/repos/o/r/actions/jobs/2/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	mux.HandleFunc("GET /api/v3/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999}}}`)
	})
	return srv
}

func ciEnvelope() ingest.Envelope {
	return ingest.Envelope{
		Schema:         ingest.EnvelopeSchema,
		DeliveryID:     "d-1",
		Kind:           ingest.KindCIFailure,
		Repo:           "o/r",
		PR:             7,
		RunID:          99,
		InstallationID: 42,
	}
}

func TestGHFetcherFetchRun(t *testing.T) {
	srv := ghAPI(t)

	f := &GHFetcher{APIBase: srv.URL}
	run, err := f.FetchRun(context.Background(), "ghs_inst", ciEnvelope())
	if err != nil {
		t.Fatalf("FetchRun: %v", err)
	}
	if len(run.Jobs) != 2 {
		t.Fatalf("got %d drillable jobs, want failed + cancelled", len(run.Jobs))
	}

	failed := run.Jobs[0]
	if failed.ID != 1 || failed.Conclusion != "failure" || len(failed.Steps) != 1 {
		t.Errorf("failed job = %+v", failed)
	}
	if failed.RawLog == "" || failed.LogError != "" {
		t.Errorf("failed job's log must download: %+v", failed)
	}

	cancelled := run.Jobs[1]
	if cancelled.ID != 2 || cancelled.RawLog != "" || cancelled.LogError == "" {
		t.Errorf("cancelled job must degrade to a LogError, got %+v", cancelled)
	}

	if run.RateRemaining != 4999 {
		t.Errorf("RateRemaining = %d, want 4999", run.RateRemaining)
	}
}

func TestGHFetcherMaxJobsCap(t *testing.T) {
	srv := ghAPI(t)

	f := &GHFetcher{APIBase: srv.URL, MaxJobs: 1}
	run, err := f.FetchRun(context.Background(), "ghs_inst", ciEnvelope())
	if err != nil {
		t.Fatalf("FetchRun: %v", err)
	}
	if len(run.Jobs) != 1 || run.Jobs[0].ID != 1 {
		t.Errorf("MaxJobs=1 must keep only the first drillable job, got %+v", run.Jobs)
	}
}

func TestGHFetcherBadRepo(t *testing.T) {
	f := &GHFetcher{}
	for _, repo := range []string{"", "own", "/r", "o/", "o/r/extra"} {
		env := ciEnvelope()
		env.Repo = repo
		if _, err := f.FetchRun(context.Background(), "tok", env); err == nil {
			t.Errorf("repo %q: want error", repo)
		}
	}
}

func TestGHFetcherRunFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no run", http.StatusNotFound)
	}))
	defer srv.Close()

	f := &GHFetcher{APIBase: srv.URL}
	if _, err := f.FetchRun(context.Background(), "tok", ciEnvelope()); err == nil {
		t.Fatal("want error when the run cannot be fetched")
	}
}

func TestSplitRepo(t *testing.T) {
	if o, n, ok := splitRepo("owner/name"); !ok || o != "owner" || n != "name" {
		t.Errorf("splitRepo(owner/name) = %q %q %v", o, n, ok)
	}
}
