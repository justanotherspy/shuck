package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/gh"
)

// DefaultGitHubURL is the public GitHub web origin, which hosts the OAuth
// authorize and token endpoints (they are not on the API host).
const DefaultGitHubURL = "https://github.com"

// GitHubAuthorizer is the GitHub App user-authorization flow: send the user
// to GitHub, exchange the returned code, resolve who they are. GitHub Apps
// do not support PKCE, so the session-bound state parameter is the whole
// CSRF defense — callers must verify it before Exchange.
type GitHubAuthorizer interface {
	AuthURL(state, redirectURI string) string
	Exchange(ctx context.Context, code, redirectURI string) (accessToken string, err error)
	User(ctx context.Context, accessToken string) (id int64, login string, err error)
}

// GitHubOAuth implements GitHubAuthorizer against real GitHub (or GHES, or
// an httptest server).
type GitHubOAuth struct {
	// ClientID and ClientSecret are the GitHub App's OAuth credentials.
	ClientID     string
	ClientSecret string
	// WebBase hosts /login/oauth/*; "" means DefaultGitHubURL.
	WebBase string
	// APIBase is the REST base for the user lookup; "" means public GitHub.
	APIBase string
	// HTTP may be nil, which means a 30s-timeout client.
	HTTP *http.Client
}

func (g *GitHubOAuth) web() string {
	if g.WebBase != "" {
		return strings.TrimSuffix(g.WebBase, "/")
	}
	return DefaultGitHubURL
}

func (g *GitHubOAuth) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// AuthURL builds the authorize redirect. No scopes: a GitHub App user token
// carries the App's permissions, so identifying the user needs none.
func (g *GitHubOAuth) AuthURL(state, redirectURI string) string {
	q := url.Values{
		"client_id":    {g.ClientID},
		"redirect_uri": {redirectURI},
		"state":        {state},
	}
	return g.web() + "/login/oauth/authorize?" + q.Encode()
}

// Exchange trades the callback code for a user access token.
func (g *GitHubOAuth) Exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.web()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange code: status %d", resp.StatusCode)
	}
	// GitHub answers 200 even for rejected codes; the error is in the body.
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode exchange response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("exchange rejected: %s (%s)", out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("exchange response has no access token")
	}
	return out.AccessToken, nil
}

// User resolves the token's identity: immutable numeric ID plus login.
func (g *GitHubOAuth) User(ctx context.Context, accessToken string) (int64, string, error) {
	client, err := gh.NewEnterprise(accessToken, g.APIBase)
	if err != nil {
		return 0, "", err
	}
	return client.AuthenticatedUser(ctx)
}
