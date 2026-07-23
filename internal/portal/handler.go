package portal

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/justanotherspy/shuck/internal/gateway"
)

// Handler serves the token portal. Wire the required fields and Register it
// on a mux; every network dependency is an interface so tests (and the dev
// loop) run without GitHub, an IdP, or DynamoDB.
type Handler struct {
	Store    TokenStore
	GitHub   GitHubAuthorizer
	Validate Validator
	// OIDC may be nil, which disables the SSO gate: GitHub authentication
	// alone gates the UI.
	OIDC     OIDCAuthenticator
	Sessions *SessionCodec
	// BaseURL is the portal's external origin (no trailing slash), the
	// base of the OAuth redirect URIs.
	BaseURL string
	// Log may be nil, which means slog.Default().
	Log *slog.Logger
	// Now may be nil, which means time.Now.
	Now func() time.Time
	// Rand may be nil, which means crypto/rand. Tests inject determinism.
	Rand io.Reader
	// Metrics may be nil, which disables counter export; every increment is
	// nil-safe.
	Metrics *Metrics
}

// Register mounts the portal's routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("GET /login", h.login)
	mux.HandleFunc("GET /oidc/callback", h.oidcCallback)
	mux.HandleFunc("GET /github/callback", h.githubCallback)
	mux.HandleFunc("POST /token", h.mintToken)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		noStore(w)
		_, _ = w.Write([]byte("ok"))
	})
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// session decodes the request's cookie; failures yield a fresh anonymous
// session (never an error page — the visitor just isn't logged in).
func (h *Handler) session(r *http.Request) Session {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return h.Sessions.Fresh()
	}
	s, err := h.Sessions.Decode(c.Value)
	if err != nil {
		return h.Sessions.Fresh()
	}
	return s
}

// setSession re-signs s onto the response. Every mutation goes through
// here, so the cookie attributes stay in one place.
func (h *Handler) setSession(w http.ResponseWriter, s Session) error {
	value, err := h.Sessions.Encode(s)
	if err != nil {
		return err
	}
	http.SetCookie(w, sessionCookie(value))
	return nil
}

// noStore marks every page uncacheable — several of them carry secrets.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// home renders the login page or, for a verified session, the dashboard.
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s := h.session(r)
	if !s.Authenticated() {
		h.render(w, http.StatusOK, "login.tmpl", loginData{OIDCEnabled: h.OIDC != nil})
		return
	}
	rows, err := h.Store.ByUser(r.Context(), s.UserID)
	if err != nil {
		h.log().Error("token listing failed", "err", err)
		h.render(w, http.StatusBadGateway, "error.tmpl", errorData{Message: "Could not load your token. Try again."})
		return
	}
	data := dashboardData{Login: s.Login, CSRF: s.CSRF}
	if len(rows) > 0 {
		data.HasToken = true
		data.Created = rows[0].Created.UTC().Format(time.RFC3339)
		if !rows[0].LastUsed.IsZero() {
			data.LastUsed = rows[0].LastUsed.UTC().Format(time.RFC3339)
		}
	}
	h.render(w, http.StatusOK, "dashboard.tmpl", data)
}

// login starts (or resumes) the auth chain: the OIDC gate first when
// enabled, then GitHub.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s := h.session(r)
	if s.Authenticated() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if h.OIDC != nil && !s.OIDCDone {
		state, err := randToken(h.Rand, 32)
		if err != nil {
			h.internalError(w, "state generation failed", err)
			return
		}
		nonce, err := randToken(h.Rand, 32)
		if err != nil {
			h.internalError(w, "nonce generation failed", err)
			return
		}
		s.OIDCState, s.OIDCNonce = state, nonce
		if err := h.setSession(w, s); err != nil {
			h.internalError(w, "session encode failed", err)
			return
		}
		http.Redirect(w, r, h.OIDC.AuthURL(state, nonce, h.BaseURL+"/oidc/callback"), http.StatusFound)
		return
	}
	state, err := randToken(h.Rand, 32)
	if err != nil {
		h.internalError(w, "state generation failed", err)
		return
	}
	s.GHState = state
	if err := h.setSession(w, s); err != nil {
		h.internalError(w, "session encode failed", err)
		return
	}
	http.Redirect(w, r, h.GitHub.AuthURL(state, h.BaseURL+"/github/callback"), http.StatusFound)
}

// oidcCallback finishes the SSO leg and bounces back into /login for the
// GitHub leg.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	s := h.session(r)
	state := r.URL.Query().Get("state")
	if !constEq(state, s.OIDCState) {
		h.forbidden(w, "oidc state mismatch")
		return
	}
	code := r.URL.Query().Get("code")
	subject, err := h.OIDC.Verify(r.Context(), code, s.OIDCNonce, h.BaseURL+"/oidc/callback")
	if err != nil {
		h.log().Warn("oidc verification failed", "err", err)
		h.forbidden(w, "oidc verification failed")
		return
	}
	h.log().Info("audit: oidc gate passed", "event", "oidc_verified", "subject", subject)
	s.OIDCDone = true
	s.OIDCState, s.OIDCNonce = "", ""
	if err := h.setSession(w, s); err != nil {
		h.internalError(w, "session encode failed", err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// githubCallback finishes identity verification: exchange the code, resolve
// the user, validate membership, and promote the session.
func (h *Handler) githubCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s := h.session(r)
	if h.OIDC != nil && !s.OIDCDone {
		h.forbidden(w, "oidc gate not passed")
		return
	}
	state := r.URL.Query().Get("state")
	if !constEq(state, s.GHState) {
		h.forbidden(w, "github state mismatch")
		return
	}
	redirectURI := h.BaseURL + "/github/callback"
	token, err := h.GitHub.Exchange(r.Context(), r.URL.Query().Get("code"), redirectURI)
	if err != nil {
		h.log().Warn("github exchange failed", "err", err)
		h.forbidden(w, "github authorization failed")
		return
	}
	userID, login, err := h.GitHub.User(r.Context(), token)
	if err != nil {
		h.log().Warn("github user lookup failed", "err", err)
		h.forbidden(w, "github identity lookup failed")
		return
	}
	if !h.membershipGate(r.Context(), w, userID, login) {
		return
	}
	csrf, err := randToken(h.Rand, 32)
	if err != nil {
		h.internalError(w, "csrf generation failed", err)
		return
	}
	// Privilege changed: rotate the whole session (fresh expiry, fresh
	// CSRF, no leftover OAuth state).
	next := h.Sessions.Fresh()
	next.OIDCDone = s.OIDCDone
	next.UserID, next.Login, next.CSRF = userID, login, csrf
	if err := h.setSession(w, next); err != nil {
		h.internalError(w, "session encode failed", err)
		return
	}
	h.log().Info("audit: github identity verified", "event", "github_verified",
		"github_user_id", userID, "github_login", login)
	http.Redirect(w, r, "/", http.StatusFound)
}

// mintToken is the POST that mints or regenerates. CSRF-guarded, and
// membership is re-validated at mint time — a session minted before an
// offboarding must not outrun the sweep.
func (h *Handler) mintToken(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s := h.session(r)
	if !s.Authenticated() {
		h.forbidden(w, "not authenticated")
		return
	}
	if err := r.ParseForm(); err != nil || !constEq(r.PostFormValue("csrf"), s.CSRF) {
		h.forbidden(w, "csrf mismatch")
		return
	}
	if !h.membershipGate(r.Context(), w, s.UserID, s.Login) {
		return
	}
	// Rotate the CSRF token before minting: a duplicate submission (double
	// click on Regenerate) then fails the CSRF check instead of racing a
	// second read-then-Replace and leaving two live tokens. Generated up
	// front so a randomness failure never strands an already-minted token.
	csrf, err := randToken(h.Rand, 32)
	if err != nil {
		h.internalError(w, "csrf rotation failed", err)
		return
	}
	raw, regenerated, err := Mint(r.Context(), h.Store, h.Rand, h.now(), s.UserID, s.Login)
	if err != nil {
		h.Metrics.incMintErrors()
		h.log().Error("mint failed", "err", err)
		h.render(w, http.StatusBadGateway, "error.tmpl", errorData{Message: "Minting failed. Your existing token, if any, is unchanged."})
		return
	}
	s.CSRF = csrf
	if err := h.setSession(w, s); err != nil {
		h.internalError(w, "session encode failed", err)
		return
	}
	event := "token_minted"
	if regenerated {
		event = "token_regenerated"
		h.Metrics.incRegenerated()
	} else {
		h.Metrics.incMinted()
	}
	h.log().Info("audit: "+event, "event", event,
		"github_user_id", s.UserID, "github_login", s.Login,
		"token_hash", gateway.HashToken(raw))
	h.render(w, http.StatusOK, "token.tmpl", tokenData{Token: raw, Regenerated: regenerated})
}

// membershipGate runs the Validator and writes the response on refusal or
// error. It reports whether the caller may proceed.
func (h *Handler) membershipGate(ctx context.Context, w http.ResponseWriter, userID int64, login string) bool {
	member, err := h.Validate.Member(ctx, userID, login)
	if err != nil {
		h.Metrics.incMembershipUnknown()
		h.log().Error("membership check failed", "github_login", login, "err", err)
		h.render(w, http.StatusBadGateway, "error.tmpl", errorData{Message: "Membership check unavailable. Try again."})
		return false
	}
	if !member {
		h.Metrics.incMembershipDenied()
		h.log().Info("audit: token refused", "event", "token_refused",
			"github_user_id", userID, "github_login", login)
		h.render(w, http.StatusForbidden, "error.tmpl", errorData{
			Message: "Your GitHub account is not authorized for this installation."})
		return false
	}
	return true
}

func (h *Handler) forbidden(w http.ResponseWriter, reason string) {
	h.log().Info("request refused", "reason", reason)
	h.render(w, http.StatusForbidden, "error.tmpl", errorData{Message: "Request refused. Start again from the home page."})
}

func (h *Handler) internalError(w http.ResponseWriter, msg string, err error) {
	h.log().Error(msg, "err", err)
	h.render(w, http.StatusInternalServerError, "error.tmpl", errorData{Message: "Internal error. Try again."})
}
