package portal

import (
	"context"
	"errors"
	"fmt"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCAuthenticator is the optional generic SSO gate in front of the portal
// UI. Any spec-compliant issuer works (Okta is the documented example); a
// nil OIDCAuthenticator on the Handler disables the gate entirely. OIDC only
// gates access to the UI — GitHub is the identity of record, and the OIDC
// subject is logged for audit but never keys a token.
type OIDCAuthenticator interface {
	AuthURL(state, nonce, redirectURI string) string
	// Verify exchanges the callback code and verifies the ID token
	// (signature, issuer, audience, expiry, nonce). It returns the token's
	// subject claim.
	Verify(ctx context.Context, code, nonce, redirectURI string) (subject string, err error)
}

// OIDCClient implements OIDCAuthenticator over go-oidc's issuer discovery.
type OIDCClient struct {
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	clientID string
	secret   string
}

// NewOIDC discovers issuer and prepares the relying party. The issuer URL
// must serve /.well-known/openid-configuration.
func NewOIDC(ctx context.Context, issuer, clientID, clientSecret string) (*OIDCClient, error) {
	provider, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer %q: %w", issuer, err)
	}
	return &OIDCClient{
		provider: provider,
		verifier: provider.Verifier(&gooidc.Config{ClientID: clientID}),
		clientID: clientID,
		secret:   clientSecret,
	}, nil
}

func (c *OIDCClient) config(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.clientID,
		ClientSecret: c.secret,
		Endpoint:     c.provider.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       []string{gooidc.ScopeOpenID},
	}
}

// AuthURL builds the authorization redirect with the session-bound state
// and nonce.
func (c *OIDCClient) AuthURL(state, nonce, redirectURI string) string {
	return c.config(redirectURI).AuthCodeURL(state, gooidc.Nonce(nonce))
}

// Verify implements OIDCAuthenticator.
func (c *OIDCClient) Verify(ctx context.Context, code, nonce, redirectURI string) (string, error) {
	tok, err := c.config(redirectURI).Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("exchange OIDC code: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return "", errors.New("token response has no id_token")
	}
	idToken, err := c.verifier.Verify(ctx, rawID)
	if err != nil {
		return "", fmt.Errorf("verify id_token: %w", err)
	}
	if idToken.Nonce != nonce {
		return "", errors.New("id_token nonce mismatch")
	}
	return idToken.Subject, nil
}
