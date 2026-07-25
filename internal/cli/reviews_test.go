package cli

import (
	"strings"
	"testing"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/model"
)

// TestReviewsExitCodeHasNothingToGateOn pins the gap the reviews usage text now
// admits to. registerInspectFlags puts --exit-code on this subcommand along
// with the rest of the shared report flags, but nothing on the reviews path can
// ever flip it: Report.HasFailures reads FailedJobs and OtherChecks, the online
// half never fetches either (prReport skips the whole CI block when reviewsOnly
// is set), and the offline half loads the full cached report and then has
// applyFocus nil them — deliberately, so `shuck reviews --offline` does not
// exit-code on somebody else's cached CI run.
//
// So the assertion here is "exit 0", which is a strange thing to want until you
// see what it guards: someone gating CI on `shuck reviews --exit-code` gets a
// permanent green and no way to notice. If a change to applyFocus (or to the
// reviews-only fetch) quietly re-enables the gate, this test fails — and the
// fix is to update the usage text with it, not to delete the assertion.
func TestReviewsExitCodeHasNothingToGateOn(t *testing.T) {
	t.Run("online never fetches a CI verdict to gate on", func(t *testing.T) {
		s := ciStub() // the PR's CI is failing
		s.fingerprint = "fp"
		s.reviews = []model.Review{{
			Author: "alice", AuthorType: model.AuthorHuman, State: "changes_requested",
			Body: "please fix the flaky test",
		}}
		withStubInspect(t, s)

		var out, errb strings.Builder
		code := runReviews([]string{"o/r", "42", "--exit-code"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — reviews carry no verdict; stderr=%q", code, errb.String())
		}
		// The CI half is not merely dropped from the render, it is never asked
		// for: that is what leaves HasFailures with nothing to see.
		if s.jobsCalls != 0 || s.otherCalls != 0 {
			t.Errorf("reviews fetched the CI half (ListJobs %d, OtherChecks %d), want neither",
				s.jobsCalls, s.otherCalls)
		}
		if !strings.Contains(out.String(), "alice") {
			t.Errorf("the reviews report is missing its review:\n%s", out.String())
		}
	})

	t.Run("offline drops the cached CI verdict instead of gating on it", func(t *testing.T) {
		t.Setenv("SHUCK_HOME", t.TempDir())
		// Both halves are in one cache entry, and both of the fields
		// HasFailures reads are populated — so an offline reviews run that kept
		// them would exit 1 on a CI failure the caller never asked about.
		report := &model.Report{
			PR:          model.PR{Owner: "o", Repo: "r", Number: 42, Title: "fix", HeadSHA: "abc1234"},
			FailedJobs:  []model.JobResult{{ID: 1, Name: "build", Conclusion: "failure", Inspected: true}},
			OtherChecks: []model.OtherCheck{{Name: "lint", Conclusion: "failure"}},
			Reviews: []model.Review{{
				Author: "alice", AuthorType: model.AuthorHuman, State: "changes_requested",
				Body: "please fix the flaky test",
			}},
		}
		if err := cache.Save(report); err != nil {
			t.Fatalf("seed cache: %v", err)
		}

		var out, errb strings.Builder
		code := runReviews([]string{"o/r", "42", "--offline", "--exit-code"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — cached CI is out of scope for reviews; stderr=%q", code, errb.String())
		}
		if strings.Contains(out.String(), "build") || strings.Contains(out.String(), "lint") {
			t.Errorf("reviews --offline leaked the cached CI report:\n%s", out.String())
		}
	})

	t.Run("the usage text says so", func(t *testing.T) {
		// A flag that is accepted and can never fire is only honest if the help
		// admits it, so the wording is part of the contract rather than a
		// comment somebody can drift away from.
		_, _, stderr := runCLI("reviews", "--help")
		for _, want := range []string{"--exit-code", "no pass/fail verdict", "shuck security --exit-code"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("`shuck reviews --help` is missing %q:\n%s", want, stderr)
			}
		}
		// The flag's own one-line description prints right below that
		// paragraph, so the shared wording ("exit 1 when failing CI checks are
		// found") would contradict it in the same screenful.
		if !strings.Contains(stderr, "never changes the exit code") {
			t.Errorf("the -exit-code flag description still promises a gate reviews cannot honor:\n%s", stderr)
		}
	})
}
