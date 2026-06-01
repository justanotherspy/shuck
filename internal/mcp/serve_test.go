package mcp

import (
	"context"
	"testing"
)

// Serve's flag parsing: a positional argument is rejected before the server
// runs, and -h returns nil (flag.ErrHelp is swallowed).
func TestServeFlagParsing(t *testing.T) {
	ctx := context.Background()
	if err := Serve(ctx, []string{"unexpected-arg"}); err == nil {
		t.Error("Serve with a positional arg err=nil, want error")
	}
	if err := Serve(ctx, []string{"-h"}); err != nil {
		t.Errorf("Serve(-h) = %v, want nil", err)
	}
}

// The inspect handlers reject invalid targets before any network call. Setting
// an empty token makes any path that slips through fail fast rather than reach
// out. Pass a nil request: the handlers ignore it.
func TestInspectHandlersErrorBeforeNetwork(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	ctx := context.Background()

	t.Run("reviews reject run url", func(t *testing.T) {
		_, _, err := inspectReviews(ctx, nil, inspectReviewsInput{URL: "https://github.com/o/r/actions/runs/5"})
		if err == nil {
			t.Error("inspectReviews with a run URL err=nil, want error")
		}
	})

	t.Run("reviews reject repo without pr", func(t *testing.T) {
		_, _, err := inspectReviews(ctx, nil, inspectReviewsInput{Repo: "o/r"})
		if err == nil {
			t.Error("inspectReviews with repo but no pr err=nil, want error")
		}
	})

	t.Run("logs reject bad run id", func(t *testing.T) {
		_, _, err := inspectLogs(ctx, nil, inspectLogsInput{Run: "garbage-no-slash-or-colon"})
		if err == nil {
			t.Error("inspectLogs with a non-numeric run err=nil, want error")
		}
	})

	t.Run("logs reject bare run id without repo", func(t *testing.T) {
		_, _, err := inspectLogs(ctx, nil, inspectLogsInput{Run: "123"})
		if err == nil {
			t.Error("inspectLogs with a bare run id and no repo err=nil, want error")
		}
	})

	t.Run("images reject registry ref without name", func(t *testing.T) {
		_, _, err := inspectImages(ctx, nil, inspectImagesInput{Image: "ghcr.io/owner"})
		if err == nil {
			t.Error("inspectImages with a nameless registry ref err=nil, want error")
		}
	})

	t.Run("images reject malformed registry ref", func(t *testing.T) {
		// A ghcr.io ref that ParseRef cannot parse errors before any fetch.
		_, _, err := inspectImages(ctx, nil, inspectImagesInput{Image: "ghcr.io/"})
		if err == nil {
			t.Error("inspectImages with a malformed registry ref err=nil, want error")
		}
	})

	t.Run("security reject invalid url", func(t *testing.T) {
		_, _, err := inspectSecurity(ctx, nil, inspectSecurityInput{URL: "://bad"})
		if err == nil {
			t.Error("inspectSecurity with an invalid URL err=nil, want error")
		}
	})

	t.Run("action reject bad ref", func(t *testing.T) {
		_, _, err := inspectAction(ctx, nil, inspectActionInput{Action: ""})
		if err == nil {
			t.Error("inspectAction with an empty ref err=nil, want error")
		}
	})
}
