package worker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKey is generated once: 2048-bit RSA keygen is too slow to repeat per
// test case.
var testKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

func pemPKCS1(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func pemPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// tokenServer serves the installation-token mint endpoint, verifying each
// request's App JWT against testKey (on the caller's fake clock) and
// counting mints.
func tokenServer(t *testing.T, now func() time.Time, expires time.Time, mints *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		if len(auth) < 8 || auth[:7] != "Bearer " {
			t.Errorf("missing bearer JWT: %q", auth)
		}
		claims := jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(auth[7:], &claims, func(tok *jwt.Token) (any, error) {
			if tok.Method != jwt.SigningMethodRS256 {
				return nil, fmt.Errorf("alg %v, want RS256", tok.Header["alg"])
			}
			return &testKey.PublicKey, nil
		}, jwt.WithTimeFunc(now))
		if err != nil {
			t.Errorf("App JWT does not verify: %v", err)
		}
		if claims.Issuer != "7" {
			t.Errorf("iss = %q, want the App ID", claims.Issuer)
		}
		if claims.IssuedAt == nil || claims.ExpiresAt == nil ||
			claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 10*time.Minute {
			t.Errorf("JWT lifetime out of GitHub's 10-minute bound: iat=%v exp=%v", claims.IssuedAt, claims.ExpiresAt)
		}
		n := mints.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_test%d","expires_at":%q}`, n, expires.Format(time.RFC3339))
	}))
}

func TestAppTokenSourceMintAndCache(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	var mints atomic.Int64
	srv := tokenServer(t, func() time.Time { return now }, now.Add(time.Hour), &mints)
	defer srv.Close()

	metrics := &Metrics{}
	src, err := NewAppTokenSource(7, pemPKCS1(testKey))
	if err != nil {
		t.Fatalf("NewAppTokenSource: %v", err)
	}
	src.BaseURL = srv.URL
	src.HTTP = srv.Client()
	src.Now = func() time.Time { return now }
	src.Metrics = metrics

	tok, err := src.Token(context.Background(), 42)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_test1" {
		t.Errorf("token = %q", tok)
	}

	// Within margin of expiry: served from cache, no second mint.
	if tok2, err := src.Token(context.Background(), 42); err != nil || tok2 != tok {
		t.Fatalf("cached Token = %q, %v", tok2, err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("server minted %d times, want 1", got)
	}
	if metrics.TokenMints.Load() != 1 || metrics.TokenCacheHits.Load() != 1 {
		t.Errorf("metrics mints=%d hits=%d, want 1/1", metrics.TokenMints.Load(), metrics.TokenCacheHits.Load())
	}

	// Advance the clock into the refresh margin: the cache must not serve a
	// nearly-expired token.
	now = now.Add(56 * time.Minute)
	tok3, err := src.Token(context.Background(), 42)
	if err != nil {
		t.Fatalf("Token after margin: %v", err)
	}
	if tok3 != "ghs_test2" {
		t.Errorf("token after margin = %q, want a fresh mint", tok3)
	}
}

func TestAppTokenSourcePKCS8Key(t *testing.T) {
	if _, err := NewAppTokenSource(7, pemPKCS8(t, testKey)); err != nil {
		t.Fatalf("PKCS#8 key rejected: %v", err)
	}
}

func TestAppTokenSourceBadInputs(t *testing.T) {
	if _, err := NewAppTokenSource(0, pemPKCS1(testKey)); err == nil {
		t.Error("want error for zero App ID")
	}
	if _, err := NewAppTokenSource(7, []byte("not a key")); err == nil {
		t.Error("want error for garbage PEM")
	}
	if _, err := NewAppTokenSource(7, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})); err == nil {
		t.Error("want error for undecodable key bytes")
	}

	src, err := NewAppTokenSource(7, pemPKCS1(testKey))
	if err != nil {
		t.Fatalf("NewAppTokenSource: %v", err)
	}
	if _, err := src.Token(context.Background(), 0); err == nil {
		t.Error("want error for zero installation id")
	}
}

func TestAppTokenSourceMintFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"non-201", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusUnauthorized)
		}},
		{"bad json", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, "{")
		}},
		{"missing token", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"expires_at":"2026-07-03T13:00:00Z"}`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			src, err := NewAppTokenSource(7, pemPKCS1(testKey))
			if err != nil {
				t.Fatalf("NewAppTokenSource: %v", err)
			}
			src.BaseURL = srv.URL
			src.HTTP = srv.Client()
			if _, err := src.Token(context.Background(), 42); err == nil {
				t.Fatal("want mint error")
			}
		})
	}
}
