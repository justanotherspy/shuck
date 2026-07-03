package worker

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultAPIBase is the public GitHub REST API base URL.
const DefaultAPIBase = "https://api.github.com"

// DefaultTokenMargin is how long before expiry a cached installation token
// stops being served. GitHub mints ~1h tokens; a 5-minute margin means a
// token handed out here survives the whole fetch pipeline that follows.
const DefaultTokenMargin = 5 * time.Minute

// appJWTLifetime is the App JWT's validity. GitHub caps it at 10 minutes;
// 9 keeps clear of the cap, and backdating iat absorbs clock skew.
const (
	appJWTLifetime = 9 * time.Minute
	appJWTBackdate = time.Minute
)

// AppTokenSource mints GitHub App installation tokens: it signs a short
// RS256 App JWT with the App private key, exchanges it at
// /app/installations/{id}/access_tokens, and caches the resulting ~1h token
// per installation until it comes within Margin of expiry. It implements
// TokenSource.
type AppTokenSource struct {
	// BaseURL is the GitHub API base; "" means DefaultAPIBase.
	BaseURL string
	// HTTP may be nil, which means a 30-second-timeout client.
	HTTP *http.Client
	// Now may be nil, which means time.Now; a fake clock in tests.
	Now func() time.Time
	// Margin is the refresh margin before token expiry; 0 means
	// DefaultTokenMargin.
	Margin time.Duration
	// Metrics may be nil, which disables counting.
	Metrics *Metrics

	appID int64
	key   *rsa.PrivateKey

	mu    sync.Mutex
	cache map[int64]installationToken
}

type installationToken struct {
	token   string
	expires time.Time
}

// NewAppTokenSource parses the App private key (PKCS#1 as GitHub serves it,
// or PKCS#8) and returns a source for the given App ID.
func NewAppTokenSource(appID int64, privateKeyPEM []byte) (*AppTokenSource, error) {
	if appID <= 0 {
		return nil, fmt.Errorf("invalid GitHub App ID %d", appID)
	}
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &AppTokenSource{appID: appID, key: key, cache: make(map[int64]installationToken)}, nil
}

// Token returns an installation token valid for at least the configured
// margin, minting a fresh one when the cache misses. Minting holds the
// source's lock — at worker QPS, single-flighting every request is simpler
// than per-installation flights and never mints the same token twice.
func (s *AppTokenSource) Token(ctx context.Context, installationID int64) (string, error) {
	if installationID <= 0 {
		return "", fmt.Errorf("invalid installation id %d", installationID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if t, ok := s.cache[installationID]; ok && t.expires.Sub(now) > s.margin() {
		s.count(func(m *Metrics) { m.TokenCacheHits.Add(1) })
		return t.token, nil
	}

	t, err := s.mint(ctx, installationID, now)
	if err != nil {
		return "", err
	}
	s.cache[installationID] = t
	s.count(func(m *Metrics) { m.TokenMints.Add(1) })
	return t.token, nil
}

// mint signs a fresh App JWT and exchanges it for an installation token.
func (s *AppTokenSource) mint(ctx context.Context, installationID int64, now time.Time) (installationToken, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(s.appID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-appJWTBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTLifetime)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.key)
	if err != nil {
		return installationToken{}, fmt.Errorf("sign App JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", s.baseURL(), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return installationToken{}, err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.http().Do(req)
	if err != nil {
		return installationToken{}, fmt.Errorf("mint token for installation %d: %w", installationID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return installationToken{}, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return installationToken{}, fmt.Errorf("mint token for installation %d: status %s", installationID, resp.Status)
	}

	var parsed struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return installationToken{}, fmt.Errorf("decode token response: %w", err)
	}
	if parsed.Token == "" {
		return installationToken{}, errors.New("token response missing token")
	}
	return installationToken{token: parsed.Token, expires: parsed.ExpiresAt}, nil
}

// parseRSAKey decodes a PEM RSA private key in either encoding GitHub App
// keys appear in.
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in App private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("App private key is %T, want RSA", parsed)
	}
	return key, nil
}

func (s *AppTokenSource) baseURL() string {
	if s.BaseURL == "" {
		return DefaultAPIBase
	}
	return s.BaseURL
}

func (s *AppTokenSource) http() *http.Client {
	if s.HTTP == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return s.HTTP
}

func (s *AppTokenSource) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *AppTokenSource) margin() time.Duration {
	if s.Margin <= 0 {
		return DefaultTokenMargin
	}
	return s.Margin
}

func (s *AppTokenSource) count(f func(*Metrics)) {
	if s.Metrics != nil {
		f(s.Metrics)
	}
}
