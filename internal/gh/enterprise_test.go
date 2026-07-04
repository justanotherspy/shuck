package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewEnterpriseEmptyBaseFallsBackToPublic(t *testing.T) {
	c, err := NewEnterprise(token, "")
	if err != nil {
		t.Fatalf("NewEnterprise: %v", err)
	}
	if got := c.gh.BaseURL(); got != "https://api.github.com/" {
		t.Errorf("base URL = %q, want public API", got)
	}
	if c.token != token {
		t.Errorf("token not retained")
	}
}

func TestNewEnterpriseRoutesToBase(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4321}}}`)
	}))
	defer srv.Close()

	c, err := NewEnterprise("inst-token", srv.URL)
	if err != nil {
		t.Fatalf("NewEnterprise: %v", err)
	}
	remaining, limit, err := c.RateRemaining(context.Background())
	if err != nil {
		t.Fatalf("RateRemaining: %v", err)
	}
	if remaining != 4321 || limit != 5000 {
		t.Errorf("got remaining=%d limit=%d, want 4321/5000", remaining, limit)
	}
	// go-github's enterprise normalization mounts the API under /api/v3/.
	if gotPath != "/api/v3/rate_limit" {
		t.Errorf("request path = %q, want /api/v3/rate_limit", gotPath)
	}
	if gotAuth != "Bearer inst-token" {
		t.Errorf("Authorization = %q, want the token as a bearer", gotAuth)
	}
}

func TestNewEnterpriseInvalidURL(t *testing.T) {
	if _, err := NewEnterprise(token, "://not-a-url"); err == nil {
		t.Fatal("want error for invalid base URL")
	}
}

func TestRateRemainingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := testClient(t, srv)
	if _, _, err := c.RateRemaining(context.Background()); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestRateRemainingNoCore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"resources":{}}`)
	}))
	defer srv.Close()

	c := testClient(t, srv)
	if _, _, err := c.RateRemaining(context.Background()); err == nil {
		t.Fatal("want error when core rate is absent")
	}
}
