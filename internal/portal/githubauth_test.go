package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGitHubAuthURL(t *testing.T) {
	g := &GitHubOAuth{ClientID: "cid", WebBase: "https://ghe.example/"}
	raw := g.AuthURL("state-1", "https://portal.example/github/callback")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if u.Host != "ghe.example" || u.Path != "/login/oauth/authorize" {
		t.Errorf("auth url = %q", raw)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("state") != "state-1" ||
		q.Get("redirect_uri") != "https://portal.example/github/callback" {
		t.Errorf("query = %v", q)
	}
	if strings.Contains(raw, "client_secret") {
		t.Error("secret leaked into the authorize URL")
	}
}

func TestGitHubExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/oauth/access_token" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q (without it GitHub answers form-encoded)", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostFormValue("client_id") != "cid" || r.PostFormValue("client_secret") != "sec" ||
			r.PostFormValue("code") != "code-1" || r.PostFormValue("redirect_uri") != "https://p.example/cb" {
			t.Errorf("form = %v", r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ghu_abc","token_type":"bearer"}`))
	}))
	defer srv.Close()

	g := &GitHubOAuth{ClientID: "cid", ClientSecret: "sec", WebBase: srv.URL}
	token, err := g.Exchange(context.Background(), "code-1", "https://p.example/cb")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if token != "ghu_abc" {
		t.Errorf("token = %q", token)
	}
}

func TestGitHubExchangeRejected(t *testing.T) {
	// GitHub answers 200 with an error body for a bad code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
	}))
	defer srv.Close()

	g := &GitHubOAuth{WebBase: srv.URL}
	if _, err := g.Exchange(context.Background(), "stale", "https://p.example/cb"); err == nil ||
		!strings.Contains(err.Error(), "bad_verification_code") {
		t.Fatalf("rejected code not surfaced: %v", err)
	}
}

func TestGitHubExchangeBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		"empty token": `{"token_type":"bearer"}`,
		"not json":    `access_token=x`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			g := &GitHubOAuth{WebBase: srv.URL}
			if _, err := g.Exchange(context.Background(), "c", "r"); err == nil {
				t.Fatal("bad response accepted")
			}
		})
	}
	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down", http.StatusBadGateway)
		}))
		defer srv.Close()
		g := &GitHubOAuth{WebBase: srv.URL}
		if _, err := g.Exchange(context.Background(), "c", "r"); err == nil {
			t.Fatal("502 accepted")
		}
	})
}

func TestGitHubUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/user") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ghu_abc" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"id": 583231, "login": "octocat"}`))
	}))
	defer srv.Close()

	g := &GitHubOAuth{APIBase: srv.URL}
	id, login, err := g.User(context.Background(), "ghu_abc")
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if id != 583231 || login != "octocat" {
		t.Errorf("user = %d %q", id, login)
	}
}
