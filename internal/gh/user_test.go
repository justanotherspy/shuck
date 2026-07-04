package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticatedUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id": 583231, "login": "octocat"}`))
	}))
	defer srv.Close()

	id, login, err := testClient(t, srv).AuthenticatedUser(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedUser: %v", err)
	}
	if id != 583231 || login != "octocat" {
		t.Errorf("got id=%d login=%q, want 583231/octocat", id, login)
	}
}

func TestAuthenticatedUserError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, _, err := testClient(t, srv).AuthenticatedUser(context.Background()); err == nil {
		t.Fatal("want error on 401, got nil")
	}
}

func TestOrgMember(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{name: "member", status: http.StatusNoContent, want: true},
		{name: "non-member", status: http.StatusNotFound, want: false},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/orgs/acme/members/octocat" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			got, err := testClient(t, srv).OrgMember(context.Background(), "acme", "octocat")
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if !strings.Contains(err.Error(), "acme") {
					t.Errorf("error %q does not name the org", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("OrgMember: %v", err)
			}
			if got != tt.want {
				t.Errorf("member = %v, want %v", got, tt.want)
			}
		})
	}
}
