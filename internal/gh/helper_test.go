package gh

import (
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v89/github"
)

// testClient wires a Client to talk to a local httptest server: the go-github
// REST base URL, the hand-rolled GraphQL endpoint, and the GHCR registry host
// all point at srv so no real network is touched. go-github resolves request
// paths against the base URL, which must end in a trailing slash.
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
		gh:          gc,
		http:        srv.Client(),
		token:       token,
		graphqlURL:  srv.URL + "/graphql",
		registryURL: srv.URL,
	}
}

// token is the dummy auth token the test client carries; some hand-rolled paths
// (GHCR token exchange) only exercise the Basic-auth branch when it is non-empty.
const token = "test-token"
