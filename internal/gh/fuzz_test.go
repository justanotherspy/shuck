package gh

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
)

// bodyRoundTripper answers every request with the same 200 body. The unit tests
// stub GitHub with an httptest server, but a fuzz worker runs tens of thousands
// of iterations whose bodies mostly fail to decode — go-github closes an
// undecodable body without draining it, so each one costs a fresh TCP
// connection and the run wedges on ephemeral ports. In-memory keeps the target
// fast and socket-free.
type bodyRoundTripper struct {
	mu   sync.Mutex
	body string
}

func (rt *bodyRoundTripper) set(body string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.body = body
}

func (rt *bodyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    req,
	}, nil
}

// zeroUnix is the Unix second of the zero time.Time (0001-01-01T00:00:00Z) —
// the instant GitHub's JSON carries for a review with no submitted_at, and what
// the oracle below treats as "never submitted".
const zeroUnix = -62135596800

// wantReviews is the fuzz oracle: given the exact bytes the reviews endpoint
// answered with, the IDs a correct PRReviewsSince must return, derived without
// calling anything the filter is built from. ok is false for a page this
// hand-written decoder cannot model exactly (a number where a string belongs, a
// timestamp go-github would accept in some other spelling), which is most of
// what a fuzzer invents; those pages fall back to the weaker properties.
func wantReviews(page string, since time.Time) (ids []int64, ok bool) {
	var elems []map[string]json.RawMessage
	if json.Unmarshal([]byte(page), &elems) != nil {
		return nil, false
	}
	for _, e := range elems {
		// encoding/json matches field names case-insensitively, so "SubMitted_At"
		// still lands in SubmittedAt; the oracle reads keys exactly, and rather
		// than reimplement that folding it declines the page.
		for k := range e {
			if lower := strings.ToLower(k); (lower == "id" || lower == "submitted_at") && lower != k {
				return nil, false
			}
		}
		var id int64
		if raw, has := e["id"]; has && json.Unmarshal(raw, &id) != nil {
			return nil, false
		}
		raw, has := e["submitted_at"]
		if !has {
			// No submitted_at at all: a draft the reviewer has not sent.
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, false
		}
		if ts.Unix() == zeroUnix && ts.Nanosecond() == 0 {
			// Explicitly serialized as the zero instant: still a draft.
			continue
		}
		if newerThan(ts, since) {
			ids = append(ids, id)
		}
	}
	return ids, true
}

// newerThan is "strictly after" spelled out in seconds and nanoseconds on
// purpose: an oracle that reused time.Time.After would move with the filter
// under any mutation of the boundary and could never disagree with it.
func newerThan(ts, since time.Time) bool {
	if a, b := ts.Unix(), since.Unix(); a != b {
		return a > b
	}
	return ts.Nanosecond() > since.Nanosecond()
}

// FuzzPRReviewsSinceFilter pins the one property the monitor's review half
// rests on: whatever the reviews endpoint answers with, PRReviewsSince hands
// back exactly the reviews that were submitted strictly after the watermark.
// Both directions are production failure modes — too loose replays a PR's
// history into a session (or delivers a reviewer's unsent draft as a verdict),
// too tight drops a reviewer's verdict on the floor — so for every page the
// oracle can model the assertion is equality, not containment.
//
// The harness serves one page per call, so nothing here can observe what a
// paginating listing does with an error mid-run; that contract is pinned by
// TestPRReviewsSinceErrorPropagates, which drives a real two-page stub.
func FuzzPRReviewsSinceFilter(f *testing.F) {
	rt := &bodyRoundTripper{body: "[]"}
	base := "http://api.invalid/"
	gc, err := github.NewClient(
		github.WithHTTPClient(&http.Client{Transport: rt}),
		github.WithURLs(&base, &base),
	)
	if err != nil {
		f.Fatalf("build github client: %v", err)
	}
	c := &Client{gh: gc, http: &http.Client{Transport: rt}, token: token}

	f.Add(`[{"id":1,"state":"APPROVED","submitted_at":"2026-05-01T10:00:00Z","user":{"id":7,"login":"alice"}}]`, int64(1777777777))
	// A watermark exactly on a review's own second: the boundary is
	// strictly-after, so review 1 must not come back a second time.
	f.Add(`[{"id":1,"submitted_at":"2026-05-01T10:00:00Z"},{"id":2,"submitted_at":"2026-05-01T10:00:05Z"}]`, int64(1777629600))
	f.Add(`[{"id":1,"state":"PENDING","user":{"login":"alice"}}]`, int64(0))
	f.Add(`[{"id":1,"submitted_at":"0001-01-01T00:00:00Z"}]`, int64(0))
	// A draft against a watermark older than the zero time: the only shape in
	// which "unsubmitted" and "not newer than since" come apart, so it is the
	// only seed that can tell the draft guard from the boundary test.
	f.Add(`[{"id":1,"state":"PENDING","user":{"login":"alice"}},{"id":2,"submitted_at":"2026-05-01T10:00:00Z"}]`, int64(-70000000000))
	f.Add(`[{"id":1,"submitted_at":"2026-05-01T10:00:00Z"},{"id":2,"submitted_at":"2026-05-01T10:00:00Z"}]`, int64(1777777777))
	f.Add(`[]`, int64(zeroUnix))
	// A spelling only encoding/json's case folding accepts, and a member the
	// review struct has no field for: the oracle must recuse itself here rather
	// than call a decoded review a draft.
	f.Add(`[{"suBmitted_At":"2027-01-01T00:00:00Z"},{"000000000000":"0"}]`, int64(1777777777))
	f.Add(`{"message":"Not Found"}`, int64(0))
	f.Add("", int64(1))
	f.Add("[{", int64(0))

	f.Fuzz(func(t *testing.T, page string, sinceUnix int64) {
		rt.set(page)
		since := time.Unix(sinceUnix, 0).UTC()
		ctx := context.Background()

		got, err := c.PRReviewsSince(ctx, "o", "r", 1, since)
		if err != nil {
			return
		}

		// For a page the oracle can decode, the answer is fully determined:
		// equality catches a filter that keeps too much and one that keeps too
		// little, and neither side of the comparison is spelled the way the
		// filter spells it.
		if want, ok := wantReviews(page, since); ok {
			if !sameIDs(reviewIDs(got), want) {
				t.Fatalf("since=%v over %s: ids = %v, want %v", since, page, reviewIDs(got), want)
			}
		}

		// Monotone in since: what survives a later watermark is a subsequence,
		// in order, of what survives the zero one. This is what still holds for
		// the pages the oracle above declines to model.
		all, err := c.PRReviewsSince(ctx, "o", "r", 1, time.Time{})
		if err != nil {
			t.Fatalf("second call failed where the first succeeded: %v", err)
		}
		i := 0
		for _, rv := range all {
			if i < len(got) && got[i] == rv {
				i++
			}
		}
		if i != len(got) {
			t.Fatalf("since=%v result %v is not a subsequence of the unfiltered %v", since, got, all)
		}
	})
}
