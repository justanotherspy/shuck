package portal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SessionCookie is the portal's only cookie. The session lives entirely
// inside it (HMAC-signed JSON) — there is no server-side session state.
const SessionCookie = "shuck_session"

// DefaultSessionTTL bounds how long a login (and any pending OAuth state)
// stays valid.
const DefaultSessionTTL = time.Hour

// MinSessionSecret is the smallest accepted HMAC key length. 32 bytes
// matches the SHA-256 output size.
const MinSessionSecret = 32

// ErrBadSession covers every way a presented cookie can fail: bad encoding,
// bad signature, expired. Deliberately one error so handlers can't leak
// which check failed.
var ErrBadSession = errors.New("portal: invalid session")

// Session is the signed cookie payload. Zero value = anonymous visitor.
type Session struct {
	// OIDCDone records that the optional OIDC gate was passed.
	OIDCDone bool `json:"oidc,omitempty"`
	// OIDCState / OIDCNonce / GHState are pending OAuth round-trip values.
	OIDCState string `json:"os,omitempty"`
	OIDCNonce string `json:"on,omitempty"`
	GHState   string `json:"gs,omitempty"`
	// UserID and Login are set only after GitHub identity verification and
	// membership validation both passed. UserID != 0 means authenticated.
	UserID int64  `json:"uid,omitempty"`
	Login  string `json:"login,omitempty"`
	// CSRF guards the token-mint POST.
	CSRF string `json:"csrf,omitempty"`
	// Expires is the unix second the session dies.
	Expires int64 `json:"exp"`
}

// Authenticated reports whether the session completed identity verification.
func (s Session) Authenticated() bool { return s.UserID != 0 }

// SessionCodec signs and verifies Session cookies.
type SessionCodec struct {
	// Secret is the HMAC-SHA256 key; at least MinSessionSecret bytes.
	Secret []byte
	// TTL falls back to DefaultSessionTTL when zero.
	TTL time.Duration
	// Now may be nil, which means time.Now.
	Now func() time.Time
}

func (c *SessionCodec) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SessionCodec) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultSessionTTL
}

// Fresh returns an empty session expiring one TTL from now.
func (c *SessionCodec) Fresh() Session {
	return Session{Expires: c.now().Add(c.ttl()).Unix()}
}

// Encode signs s into its cookie value: b64url(json) + "." + b64url(mac).
func (c *SessionCodec) Encode(s Session) (string, error) {
	if len(c.Secret) < MinSessionSecret {
		return "", fmt.Errorf("session secret is %d bytes, need at least %d", len(c.Secret), MinSessionSecret)
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode session: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, c.Secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Decode verifies a cookie value and returns its session. Any failure —
// encoding, signature, expiry — is ErrBadSession.
func (c *SessionCodec) Decode(value string) (Session, error) {
	if len(c.Secret) < MinSessionSecret {
		return Session{}, ErrBadSession
	}
	body, sig, ok := strings.Cut(value, ".")
	if !ok {
		return Session{}, ErrBadSession
	}
	// Strict decoding rejects non-canonical encodings (non-zero unused
	// trailing bits), so every accepted cookie has exactly one byte form —
	// no malleability. Found by FuzzPortalSessionCodec.
	got, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil {
		return Session{}, ErrBadSession
	}
	mac := hmac.New(sha256.New, c.Secret)
	mac.Write([]byte(body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return Session{}, ErrBadSession
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return Session{}, ErrBadSession
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}, ErrBadSession
	}
	if s.Expires <= c.now().Unix() {
		return Session{}, ErrBadSession
	}
	return s, nil
}

// cookie wraps an encoded session value in the portal's cookie attributes.
// SameSite=Lax (not Strict) because the OAuth providers return the user via
// top-level cross-site redirects, which must carry the cookie.
func sessionCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// randToken returns n random bytes from r as unpadded base64url — the shape
// of every state, nonce, and CSRF value. A nil r means crypto/rand.
func randToken(r io.Reader, n int) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// constEq is a constant-time string equality that also rejects empty values
// (an unset state/CSRF must never match an empty parameter).
func constEq(a, b string) bool {
	return a != "" && b != "" && hmac.Equal([]byte(a), []byte(b))
}
