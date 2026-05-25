package gh

import (
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestClassifyAuthor(t *testing.T) {
	tests := []struct {
		login    string
		typename string
		want     model.AuthorType
	}{
		{"alice", "User", model.AuthorHuman},
		{"github-actions[bot]", "Bot", model.AuthorBot},
		{"dependabot", "Bot", model.AuthorBot},
		{"renovate[bot]", "User", model.AuthorBot}, // login suffix wins even when typed User
		{"copilot-pull-request-reviewer[bot]", "Bot", model.AuthorAI},
		{"coderabbitai[bot]", "Bot", model.AuthorAI},
		{"Claude", "User", model.AuthorAI}, // case-insensitive
		{"", "", model.AuthorHuman},
	}
	for _, tc := range tests {
		if got := classifyAuthor(tc.login, tc.typename); got != tc.want {
			t.Errorf("classifyAuthor(%q, %q) = %q, want %q", tc.login, tc.typename, got, tc.want)
		}
	}
}

func TestBuildReviewsGroupsAndNormalizes(t *testing.T) {
	reviews := []rawReview{
		{ID: "R1", State: "CHANGES_REQUESTED", Body: "fix this", Author: gqlActor{Login: "bob", Typename: "User"}},
		{ID: "R2", State: "APPROVED", Body: "lgtm", Author: gqlActor{Login: "alice", Typename: "User"}},
		{ID: "R3", State: "PENDING", Body: "wip", Author: gqlActor{Login: "carol", Typename: "User"}},
	}
	threads := []rawThread{
		{
			Path: "main.go", Line: 10,
			Comments: []rawThreadComment{
				{Body: "needs a nil check", Author: gqlActor{Login: "bob"}, ReviewID: "R1"},
				{Body: "agreed", Author: gqlActor{Login: "alice"}, ReviewID: "R1"},
			},
		},
		{
			Path: "orphan.go", Line: 1,
			Comments: []rawThreadComment{
				{Body: "dangling", Author: gqlActor{Login: "x"}, ReviewID: "R99"}, // no matching review -> dropped
			},
		},
	}

	got := buildReviews(reviews, threads, 5)

	if len(got) != 2 {
		t.Fatalf("want 2 reviews (PENDING skipped), got %d", len(got))
	}
	if got[0].State != "changes_requested" {
		t.Errorf("state[0] = %q, want changes_requested", got[0].State)
	}
	if len(got[0].Threads) != 1 {
		t.Fatalf("review R1 should have 1 thread, got %d", len(got[0].Threads))
	}
	if n := len(got[0].Threads[0].Comments); n != 2 {
		t.Errorf("thread comments = %d, want 2", n)
	}
	if len(got[1].Threads) != 0 {
		t.Errorf("review R2 should have no threads, got %d", len(got[1].Threads))
	}
}

func TestBuildReviewsDropsEmptyCommented(t *testing.T) {
	reviews := []rawReview{
		{ID: "R1", State: "COMMENTED", Body: "", Author: gqlActor{Login: "bob"}},                  // empty -> dropped
		{ID: "R2", State: "APPROVED", Body: "", Author: gqlActor{Login: "alice"}},                 // empty approve -> kept
		{ID: "R3", State: "COMMENTED", Body: "actually useful", Author: gqlActor{Login: "carol"}}, // has body -> kept
	}
	got := buildReviews(reviews, nil, 5)
	if len(got) != 2 {
		t.Fatalf("want 2 reviews (empty commented dropped), got %d: %+v", len(got), got)
	}
	if got[0].State != "approved" || got[1].State != "commented" {
		t.Errorf("unexpected surviving reviews: %+v", got)
	}
}

func TestSummarizeThreadCollapse(t *testing.T) {
	resolved := summarizeThread(rawThread{
		Path: "a.go", Line: 5, IsResolved: true, ResolvedBy: "bob",
		Comments: []rawThreadComment{{Body: "hidden", ReviewID: "R1"}},
	}, 5)
	if !resolved.Collapsed || resolved.CollapseReason != "resolved by bob" {
		t.Errorf("resolved collapse = %+v", resolved)
	}
	if len(resolved.Comments) != 0 {
		t.Errorf("collapsed thread must not include comments, got %d", len(resolved.Comments))
	}

	outdated := summarizeThread(rawThread{
		Path: "a.go", IsOutdated: true,
		Comments: []rawThreadComment{{Body: "hidden", ReviewID: "R1"}},
	}, 5)
	if !outdated.Collapsed || outdated.CollapseReason != "outdated" {
		t.Errorf("outdated collapse = %+v", outdated)
	}

	resolvedNoUser := summarizeThread(rawThread{IsResolved: true, Comments: []rawThreadComment{{ReviewID: "R1"}}}, 5)
	if resolvedNoUser.CollapseReason != "resolved" {
		t.Errorf("reason = %q, want resolved", resolvedNoUser.CollapseReason)
	}
}

func TestSummarizeThreadLimit(t *testing.T) {
	mk := func(n int) []rawThreadComment {
		out := make([]rawThreadComment, n)
		for i := range out {
			out[i] = rawThreadComment{Body: "c", ReviewID: "R1"}
		}
		return out
	}

	got := summarizeThread(rawThread{Path: "a.go", Comments: mk(8)}, 3)
	if len(got.Comments) != 3 {
		t.Errorf("comments shown = %d, want 3", len(got.Comments))
	}
	if got.HiddenComments != 5 {
		t.Errorf("hidden = %d, want 5", got.HiddenComments)
	}
	if got.TotalComments != 8 {
		t.Errorf("total = %d, want 8", got.TotalComments)
	}

	// A limit below 1 still keeps the first comment.
	clamped := summarizeThread(rawThread{Path: "a.go", Comments: mk(2)}, 0)
	if len(clamped.Comments) != 1 || clamped.HiddenComments != 1 {
		t.Errorf("clamp result = %d shown, %d hidden; want 1 shown, 1 hidden", len(clamped.Comments), clamped.HiddenComments)
	}

	// Fewer comments than the limit hides nothing.
	under := summarizeThread(rawThread{Path: "a.go", Comments: mk(2)}, 5)
	if under.HiddenComments != 0 {
		t.Errorf("hidden = %d, want 0", under.HiddenComments)
	}
}
