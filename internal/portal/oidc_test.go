package portal

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIssuer is a minimal OIDC provider: discovery, JWKS, and a token
// endpoint returning an id_token signed with a test RSA key.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey // the key JWKS advertises

	mu      sync.Mutex
	signKey *rsa.PrivateKey // the key /token signs with (defaults to key)
	claims  jwt.MapClaims   // template for the next id_token
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIssuer{key: key, signKey: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		pub := &f.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"alg": "RS256",
				"use": "sig",
				"kid": "test",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		claims := jwt.MapClaims{}
		for k, v := range f.claims {
			claims[k] = v
		}
		signKey := f.signKey
		f.mu.Unlock()
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "test"
		signed, err := tok.SignedString(signKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at",
			"token_type":   "bearer",
			"id_token":     signed,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// setClaims primes the next id_token; zero exp/iat get sane defaults.
func (f *fakeIssuer) setClaims(nonce string, mutate func(jwt.MapClaims)) {
	claims := jwt.MapClaims{
		"iss":   f.srv.URL,
		"aud":   "client-1",
		"sub":   "user-9",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"nonce": nonce,
	}
	if mutate != nil {
		mutate(claims)
	}
	f.mu.Lock()
	f.claims = claims
	f.mu.Unlock()
}

func newTestOIDC(t *testing.T, issuer *fakeIssuer) *OIDCClient {
	t.Helper()
	client, err := NewOIDC(context.Background(), issuer.srv.URL, "client-1", "secret-1")
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return client
}

func TestOIDCAuthURL(t *testing.T) {
	issuer := newFakeIssuer(t)
	client := newTestOIDC(t, issuer)
	raw := client.AuthURL("state-1", "nonce-1", "https://p.example/oidc/callback")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("state") != "state-1" || q.Get("nonce") != "nonce-1" ||
		q.Get("client_id") != "client-1" || !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("query = %v", q)
	}
	if strings.Contains(raw, "secret-1") {
		t.Error("client secret leaked into the authorize URL")
	}
}

func TestOIDCVerify(t *testing.T) {
	issuer := newFakeIssuer(t)
	client := newTestOIDC(t, issuer)
	issuer.setClaims("nonce-1", nil)
	subject, err := client.Verify(context.Background(), "code-1", "nonce-1", "https://p.example/cb")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if subject != "user-9" {
		t.Errorf("subject = %q", subject)
	}
}

func TestOIDCVerifyRejects(t *testing.T) {
	tests := []struct {
		name   string
		nonce  string // nonce presented to Verify
		mutate func(jwt.MapClaims)
	}{
		{name: "nonce mismatch", nonce: "other", mutate: nil},
		{name: "expired", nonce: "nonce-1", mutate: func(c jwt.MapClaims) {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
		}},
		{name: "wrong audience", nonce: "nonce-1", mutate: func(c jwt.MapClaims) {
			c["aud"] = "someone-else"
		}},
		{name: "wrong issuer", nonce: "nonce-1", mutate: func(c jwt.MapClaims) {
			c["iss"] = "https://evil.example"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newFakeIssuer(t)
			client := newTestOIDC(t, issuer)
			issuer.setClaims("nonce-1", tt.mutate)
			if _, err := client.Verify(context.Background(), "code", tt.nonce, "r"); err == nil {
				t.Fatal("verification passed, want failure")
			}
		})
	}
}

func TestOIDCDiscoveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := NewOIDC(context.Background(), srv.URL, "c", "s"); err == nil {
		t.Fatal("discovery failure accepted")
	}
}

// Guard against the fake drifting: a token signed by a key the JWKS does
// not advertise must fail signature verification.
func TestOIDCVerifyWrongKey(t *testing.T) {
	issuer := newFakeIssuer(t)
	client := newTestOIDC(t, issuer)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer.mu.Lock()
	issuer.signKey = otherKey
	issuer.mu.Unlock()
	issuer.setClaims("nonce-1", nil)
	if _, err := client.Verify(context.Background(), "code", "nonce-1", "r"); err == nil {
		t.Fatal("token signed by unknown key verified")
	}
}
