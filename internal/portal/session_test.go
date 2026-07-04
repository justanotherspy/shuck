package portal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testCodec() *SessionCodec {
	return &SessionCodec{
		Secret: []byte("0123456789abcdef0123456789abcdef"),
		Now:    func() time.Time { return time.Unix(1_000_000, 0) },
	}
}

func TestSessionRoundTrip(t *testing.T) {
	c := testCodec()
	want := Session{
		OIDCDone: true,
		GHState:  "state-1",
		UserID:   42,
		Login:    "octocat",
		CSRF:     "csrf-1",
		Expires:  c.now().Add(time.Hour).Unix(),
	}
	value, err := c.Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := c.Decode(value)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestSessionExpiryRejected(t *testing.T) {
	c := testCodec()
	value, err := c.Encode(Session{UserID: 1, Expires: c.now().Unix()})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := c.Decode(value); !errors.Is(err, ErrBadSession) {
		t.Fatalf("expired session decoded: %v", err)
	}
}

func TestSessionTamperRejected(t *testing.T) {
	c := testCodec()
	value, err := c.Encode(Session{UserID: 1, Expires: c.now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i := 0; i < len(value); i++ {
		if value[i] == '.' {
			continue
		}
		mutated := []byte(value)
		mutated[i] ^= 0x01
		if _, err := c.Decode(string(mutated)); err == nil {
			t.Fatalf("tampered byte %d decoded", i)
		}
	}
	if _, err := c.Decode(strings.TrimSuffix(value, value[len(value)-4:])); err == nil {
		t.Fatal("truncated value decoded")
	}
	if _, err := c.Decode("garbage"); err == nil {
		t.Fatal("undelimited garbage decoded")
	}
}

func TestSessionWrongSecretRejected(t *testing.T) {
	c := testCodec()
	value, err := c.Encode(Session{UserID: 1, Expires: c.now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	other := testCodec()
	other.Secret = []byte("fedcba9876543210fedcba9876543210")
	if _, err := other.Decode(value); !errors.Is(err, ErrBadSession) {
		t.Fatalf("cross-secret decode: %v", err)
	}
}

func TestSessionShortSecretRefused(t *testing.T) {
	c := &SessionCodec{Secret: []byte("short")}
	if _, err := c.Encode(Session{Expires: 1}); err == nil {
		t.Fatal("short secret accepted for encode")
	}
	if _, err := c.Decode("a.b"); !errors.Is(err, ErrBadSession) {
		t.Fatal("short secret accepted for decode")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	c := sessionCookie("v")
	if !c.Secure || !c.HttpOnly {
		t.Errorf("cookie must be Secure+HttpOnly: %+v", c)
	}
	if c.SameSite != 2 { // http.SameSiteLaxMode
		t.Errorf("cookie SameSite = %v, want Lax (OAuth redirects must carry it)", c.SameSite)
	}
	if c.Path != "/" || c.Name != SessionCookie {
		t.Errorf("cookie name/path = %q %q", c.Name, c.Path)
	}
}

func TestConstEqRejectsEmpty(t *testing.T) {
	if constEq("", "") {
		t.Fatal("empty values must never match (unset state vs missing param)")
	}
	if !constEq("a", "a") || constEq("a", "b") {
		t.Fatal("constEq broken")
	}
}

func TestFreshUsesTTL(t *testing.T) {
	c := testCodec()
	c.TTL = 2 * time.Hour
	if got := c.Fresh().Expires; got != c.now().Add(2*time.Hour).Unix() {
		t.Errorf("Fresh expires = %d", got)
	}
	c.TTL = 0
	if got := c.Fresh().Expires; got != c.now().Add(DefaultSessionTTL).Unix() {
		t.Errorf("Fresh default expires = %d", got)
	}
}
