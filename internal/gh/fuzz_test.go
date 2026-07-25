package gh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// FuzzPRReviewsSinceFilter pins the one property the monitor's review half
// rests on: whatever the reviews endpoint answers with, PRReviewsSince hands
// back only reviews that were actually submitted strictly after the watermark,
// and raising the watermark only ever removes reviews. Both directions are
// failure modes in production — too loose replays a PR's history into a
// session, too tight drops a reviewer's verdict on the floor.
func FuzzPRReviewsSinceFilter(f *testing.F) {
	var (
		mu   sync.Mutex
		body = "[]"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f.Add(`[{"id":1,"state":"APPROVED","submitted_at":"2026-05-01T10:00:00Z","user":{"id":7,"login":"alice"}}]`, int64(1777777777))
	f.Add(`[{"id":1,"state":"PENDING","user":{"login":"alice"}}]`, int64(0))
	f.Add(`[{"id":1,"submitted_at":"0001-01-01T00:00:00Z"}]`, int64(0))
	f.Add(`[]`, int64(-62135596800))
	f.Add(`{"message":"Not Found"}`, int64(0))
	f.Add("", int64(1))
	f.Add("[{", int64(0))

	f.Fuzz(func(t *testing.T, page string, sinceUnix int64) {
		mu.Lock()
		body = page
		mu.Unlock()

		since := time.Unix(sinceUnix, 0).UTC()
		c := testClient(t, srv)
		ctx := context.Background()

		got, err := c.PRReviewsSince(ctx, "o", "r", 1, since)
		if err != nil {
			// A failed listing must never masquerade as "no new reviews".
			if got != nil {
				t.Fatalf("got %+v alongside error %v, want nil", got, err)
			}
			return
		}
		for _, rv := range got {
			if rv.SubmittedAt.IsZero() {
				t.Fatalf("unsubmitted review %d survived the filter", rv.ID)
			}
			if !rv.SubmittedAt.After(since) {
				t.Fatalf("review %d submitted %v is not after since %v", rv.ID, rv.SubmittedAt, since)
			}
		}

		// Never more results than the endpoint offered: the filter may drop
		// reviews, it may not invent them.
		var raw []json.RawMessage
		if json.Unmarshal([]byte(page), &raw) == nil && len(got) > len(raw) {
			t.Fatalf("got %d reviews from a %d-element page", len(got), len(raw))
		}

		// Monotone in since: what survives a later watermark is a subsequence
		// of what survives an earlier one, in the same order.
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
