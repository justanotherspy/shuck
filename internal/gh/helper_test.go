package gh

import (
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v89/github"
)

// testClient wires a Client to talk to a local httptest server: both the
// go-github REST base URL and the hand-rolled GraphQL endpoint point at srv, so
// no real network is touched. go-github resolves request paths against the base
// URL, which must end in a trailing slash.
func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	base := srv.URL + "/"
	gc, err := github.NewClient(
		github.WithHTTPClient(srv.Client()),
		github.WithURLs(&base, &base),
	)
	if err != nil {
		t.Fatalf("build github client: %v", err)
	}
	return &Client{
		gh:         gc,
		http:       srv.Client(),
		token:      token,
		graphqlURL: srv.URL + "/graphql",
	}
}

// token is the dummy auth token the test client carries. The GraphQL path is
// hand-rolled and sets its own Authorization header from this field, so it has
// to be non-empty for those tests to exercise the authenticated branch.
const token = "test-token"
