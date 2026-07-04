package portal

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzPortalSessionCodec asserts the codec's two invariants: a session
// round-trips to identity under its own secret before expiry, and no
// mutation of the encoded value — nor any input at all under a different
// secret — ever decodes successfully. The session cookie is the portal's
// entire auth state, so this must fail closed.
func FuzzPortalSessionCodec(f *testing.F) {
	f.Add("octocat", int64(42), "csrf-1", true, 0, byte(0x01))
	f.Add("", int64(0), "", false, 5, byte(0xFF))
	f.Add("a#b.c", int64(-1), "x", true, 100, byte(0x80))
	f.Fuzz(func(t *testing.T, login string, userID int64, csrf string, oidc bool, mutateAt int, mutateBy byte) {
		if !utf8.ValidString(login) || !utf8.ValidString(csrf) {
			// encoding/json coerces invalid UTF-8 to U+FFFD, so identity
			// only holds for valid strings — which is every real session
			// value (base64url randomness and GitHub JSON logins).
			t.Skip()
		}
		now := time.Unix(1_000_000, 0)
		codec := &SessionCodec{
			Secret: bytes.Repeat([]byte{0x42}, 32),
			Now:    func() time.Time { return now },
		}
		want := Session{
			OIDCDone: oidc,
			UserID:   userID,
			Login:    login,
			CSRF:     csrf,
			Expires:  now.Add(time.Hour).Unix(),
		}
		value, err := codec.Encode(want)
		if err != nil {
			t.Fatalf("Encode(%+v): %v", want, err)
		}

		// Round trip is identity.
		got, err := codec.Decode(value)
		if err != nil {
			t.Fatalf("Decode of freshly encoded session failed: %v", err)
		}
		if got != want {
			t.Fatalf("round trip = %+v, want %+v", got, want)
		}

		// Any single-byte mutation fails closed.
		if mutateBy != 0 && value != "" {
			i := ((mutateAt % len(value)) + len(value)) % len(value)
			mutated := []byte(value)
			mutated[i] ^= mutateBy
			if string(mutated) != value {
				if _, err := codec.Decode(string(mutated)); err == nil {
					t.Fatalf("mutated cookie decoded (byte %d ^= %#x)", i, mutateBy)
				}
			}
		}

		// No value decodes under a different secret.
		other := &SessionCodec{
			Secret: bytes.Repeat([]byte{0x43}, 32),
			Now:    func() time.Time { return now },
		}
		if _, err := other.Decode(value); err == nil {
			t.Fatal("cookie decoded under a different secret")
		}
	})
}

// FuzzPortalCallbackParams throws arbitrary state/code/cookie values at the
// OAuth callback endpoints. Invariants: no panic, no callback ever
// authenticates the session without an exact state match, and no response
// ever carries a raw shk_ token.
func FuzzPortalCallbackParams(f *testing.F) {
	f.Add("state", "code", "cookie", "/github/callback")
	f.Add("", "", "", "/oidc/callback")
	f.Add("a&b=c", "%%", "shuck_session=x.y", "/github/callback")
	f.Add("\x00\xff", "code", "..", "/oidc/callback")
	f.Fuzz(func(t *testing.T, state, code, cookie, path string) {
		if path != "/github/callback" && path != "/oidc/callback" {
			path = "/github/callback"
		}
		store := newFakeStore()
		h := &Handler{
			Store:    store,
			GitHub:   &fakeGitHub{userID: 42, login: "octocat"},
			Validate: &fakeValidator{member: true},
			OIDC:     &fakeOIDC{},
			Sessions: &SessionCodec{Secret: bytes.Repeat([]byte{0x42}, 32)},
			BaseURL:  "https://portal.example",
			Log:      slog.New(slog.DiscardHandler),
		}
		mux := http.NewServeMux()
		h.Register(mux)

		q := url.Values{"state": {state}, "code": {code}}
		req := httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), http.NoBody)
		if cookie != "" {
			req.Header.Set("Cookie", SessionCookie+"="+cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req) // must not panic

		resp := rec.Result()
		defer resp.Body.Close()

		// The fuzzer never holds a signed session carrying a matching
		// state (the cookie is attacker-controlled bytes), so no callback
		// may succeed: a GitHub callback success is a 302 to "/".
		if path == "/github/callback" && resp.StatusCode == http.StatusFound {
			t.Fatalf("github callback authenticated with state=%q cookie=%q", state, cookie)
		}
		if resp.StatusCode == http.StatusInternalServerError {
			t.Fatalf("callback answered 500 for state=%q code=%q cookie=%q", state, code, cookie)
		}
		if strings.Contains(rec.Body.String(), TokenPrefix) {
			t.Fatal("callback response carries a raw token")
		}
		if store.count() != 0 {
			t.Fatal("callback minted a token")
		}
	})
}
