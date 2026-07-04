package portal

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// capturingLog collects every rendered log line so tests can assert what
// never appears in them.
type capturingLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturingLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&syncWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (c *capturingLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type syncWriter struct{ c *capturingLog }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}

// portalRig is a fully wired Handler on an httptest server with a
// cookie-jar-free manual client (redirects not followed, cookies threaded by
// hand so tests can inspect every hop).
type portalRig struct {
	t       *testing.T
	srv     *httptest.Server
	handler *Handler
	store   *fakeStore
	github  *fakeGitHub
	oidc    *fakeOIDC
	valid   *fakeValidator
	log     *capturingLog
	cookie  string // current session cookie value
}

func newRig(t *testing.T, withOIDC bool) *portalRig {
	t.Helper()
	rig := &portalRig{
		t:      t,
		store:  newFakeStore(),
		github: &fakeGitHub{userID: 42, login: "octocat"},
		valid:  &fakeValidator{member: true},
		log:    &capturingLog{},
	}
	h := &Handler{
		Store:    rig.store,
		GitHub:   rig.github,
		Validate: rig.valid,
		Sessions: &SessionCodec{Secret: []byte("0123456789abcdef0123456789abcdef")},
		BaseURL:  "https://portal.example",
		Log:      rig.log.logger(),
	}
	if withOIDC {
		rig.oidc = &fakeOIDC{}
		h.OIDC = rig.oidc
	}
	rig.handler = h
	mux := http.NewServeMux()
	h.Register(mux)
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

// do sends one request, carrying and re-capturing the session cookie.
func (r *portalRig) do(method, path string, form url.Values) *http.Response {
	r.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, r.srv.URL+path, body)
	if err != nil {
		r.t.Fatalf("build request: %v", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: r.cookie})
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			r.cookie = c.Value
		}
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// stateFrom pulls the state query param out of a Location header.
func stateFrom(t *testing.T, resp *http.Response) string {
	t.Helper()
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("no redirect location: %v", err)
	}
	return loc.Query().Get("state")
}

var tokenInBody = regexp.MustCompile(`shk_[A-Za-z0-9_-]{43}`)

// login walks the full GitHub-only happy path and leaves the rig with an
// authenticated session.
func (r *portalRig) login() {
	r.t.Helper()
	resp := r.do(http.MethodGet, "/login", nil)
	if resp.StatusCode != http.StatusFound {
		r.t.Fatalf("/login status = %d", resp.StatusCode)
	}
	state := stateFrom(r.t, resp)
	resp = r.do(http.MethodGet, "/github/callback?state="+url.QueryEscape(state)+"&code=code-1", nil)
	if resp.StatusCode != http.StatusFound {
		r.t.Fatalf("github callback status = %d: %s", resp.StatusCode, readBody(r.t, resp))
	}
}

// csrf reads the CSRF token off the dashboard form.
func (r *portalRig) csrf() string {
	r.t.Helper()
	body := readBody(r.t, r.do(http.MethodGet, "/", nil))
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		r.t.Fatalf("no csrf field on dashboard:\n%s", body)
	}
	return m[1]
}

func TestFullFlowWithoutOIDC(t *testing.T) {
	rig := newRig(t, false)

	// Anonymous home: login page, no dashboard.
	body := readBody(t, rig.do(http.MethodGet, "/", nil))
	if !strings.Contains(body, "Connect GitHub") {
		t.Fatalf("anonymous home is not the login page:\n%s", body)
	}

	rig.login()

	// Dashboard: no token yet.
	body = readBody(t, rig.do(http.MethodGet, "/", nil))
	if !strings.Contains(body, "octocat") || !strings.Contains(body, "no token yet") {
		t.Fatalf("dashboard wrong:\n%s", body)
	}

	// Mint. Raw token in the body, exactly there and nowhere else.
	resp := rig.do(http.MethodPost, "/token", url.Values{"csrf": {rig.csrf()}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("token page is cacheable")
	}
	body = readBody(t, resp)
	raw := tokenInBody.FindString(body)
	if raw == "" {
		t.Fatalf("no token in mint response:\n%s", body)
	}
	if rig.store.count() != 1 {
		t.Fatalf("store rows = %d", rig.store.count())
	}
	if strings.Contains(rig.log.String(), raw) {
		t.Fatal("raw token leaked into logs")
	}

	// Dashboard now shows the token's existence, never its value.
	body = readBody(t, rig.do(http.MethodGet, "/", nil))
	if !strings.Contains(body, "active token") {
		t.Fatalf("dashboard after mint:\n%s", body)
	}
	if tokenInBody.MatchString(body) {
		t.Fatal("raw token re-shown on dashboard")
	}

	// Regenerate: old row gone, new row present, audit logged.
	resp = rig.do(http.MethodPost, "/token", url.Values{"csrf": {rig.csrf()}})
	body = readBody(t, resp)
	raw2 := tokenInBody.FindString(body)
	if raw2 == "" || raw2 == raw {
		t.Fatalf("regenerate did not mint a fresh token")
	}
	if rig.store.count() != 1 {
		t.Fatalf("store rows after regenerate = %d, want 1 (old revoked)", rig.store.count())
	}
	if !strings.Contains(rig.log.String(), "token_regenerated") {
		t.Error("regenerate not audited")
	}
}

func TestFullFlowWithOIDC(t *testing.T) {
	rig := newRig(t, true)

	// /login goes to the IdP first.
	resp := rig.do(http.MethodGet, "/login", nil)
	loc, err := resp.Location()
	if err != nil || loc.Host != "idp.example" {
		t.Fatalf("first hop = %v, want the IdP", loc)
	}
	oidcState := loc.Query().Get("state")
	nonce := loc.Query().Get("nonce")

	// Skipping the OIDC gate by calling the GitHub callback directly fails.
	forbidden := rig.do(http.MethodGet, "/github/callback?state=x&code=c", nil)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("github callback without OIDC = %d, want 403", forbidden.StatusCode)
	}

	// OIDC callback with the right state passes the gate.
	resp = rig.do(http.MethodGet, "/oidc/callback?state="+url.QueryEscape(oidcState)+"&code=c1", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("oidc callback = %d", resp.StatusCode)
	}
	if rig.oidc.gotNonce != nonce {
		t.Errorf("verify nonce = %q, want the one from the auth URL", rig.oidc.gotNonce)
	}

	// Second /login hop goes to GitHub now.
	resp = rig.do(http.MethodGet, "/login", nil)
	state := stateFrom(t, resp)
	resp = rig.do(http.MethodGet, "/github/callback?state="+url.QueryEscape(state)+"&code=code-1", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("github callback = %d", resp.StatusCode)
	}
	body := readBody(t, rig.do(http.MethodGet, "/", nil))
	if !strings.Contains(body, "octocat") {
		t.Fatalf("not signed in after full OIDC+GitHub flow:\n%s", body)
	}
}

func TestOIDCStateMismatch(t *testing.T) {
	rig := newRig(t, true)
	rig.do(http.MethodGet, "/login", nil)
	resp := rig.do(http.MethodGet, "/oidc/callback?state=wrong&code=c", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestGitHubStateMismatch(t *testing.T) {
	rig := newRig(t, false)
	rig.do(http.MethodGet, "/login", nil)
	resp := rig.do(http.MethodGet, "/github/callback?state=wrong&code=c", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if rig.github.gotCode != "" {
		t.Fatal("code exchanged despite state mismatch")
	}
}

func TestCallbackWithoutSession(t *testing.T) {
	rig := newRig(t, false)
	// No prior /login: fresh session has no state, nothing can match.
	resp := rig.do(http.MethodGet, "/github/callback?state=&code=c", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestNonMemberRefused(t *testing.T) {
	rig := newRig(t, false)
	rig.valid.member = false
	resp := rig.do(http.MethodGet, "/login", nil)
	state := stateFrom(t, resp)
	resp = rig.do(http.MethodGet, "/github/callback?state="+url.QueryEscape(state)+"&code=c", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member callback = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "not authorized") {
		t.Error("refusal page missing explanation")
	}
	if !strings.Contains(rig.log.String(), "token_refused") {
		t.Error("refusal not audited")
	}
	// And the session did not become authenticated.
	body := readBody(t, rig.do(http.MethodGet, "/", nil))
	if !strings.Contains(body, "Connect GitHub") {
		t.Fatal("non-member ended up signed in")
	}
}

func TestValidatorErrorIsNotARefusalNorAPass(t *testing.T) {
	rig := newRig(t, false)
	rig.valid.err = errors.New("github down")
	resp := rig.do(http.MethodGet, "/login", nil)
	state := stateFrom(t, resp)
	resp = rig.do(http.MethodGet, "/github/callback?state="+url.QueryEscape(state)+"&code=c", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("validator error = %d, want 502", resp.StatusCode)
	}
}

func TestMintRequiresAuthAndCSRF(t *testing.T) {
	rig := newRig(t, false)

	// Unauthenticated.
	resp := rig.do(http.MethodPost, "/token", url.Values{"csrf": {"x"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated mint = %d", resp.StatusCode)
	}

	rig.login()

	// Missing CSRF.
	resp = rig.do(http.MethodPost, "/token", url.Values{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf = %d", resp.StatusCode)
	}
	// Wrong CSRF.
	resp = rig.do(http.MethodPost, "/token", url.Values{"csrf": {"wrong"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong csrf = %d", resp.StatusCode)
	}
	if rig.store.count() != 0 {
		t.Fatal("a refused mint wrote a row")
	}
}

func TestMintRevalidatesMembership(t *testing.T) {
	rig := newRig(t, false)
	rig.login()
	csrf := rig.csrf()

	// Offboarded between login and mint: the mint must refuse.
	rig.valid.member = false
	resp := rig.do(http.MethodPost, "/token", url.Values{"csrf": {csrf}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("offboarded mint = %d, want 403", resp.StatusCode)
	}
	if rig.store.count() != 0 {
		t.Fatal("offboarded user minted a token")
	}
}

func TestMintStoreFailureKeepsExistingToken(t *testing.T) {
	rig := newRig(t, false)
	rig.login()
	csrf := rig.csrf()
	rig.store.writeErr = errors.New("transact down")
	resp := rig.do(http.MethodPost, "/token", url.Values{"csrf": {csrf}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("store failure = %d, want 502", resp.StatusCode)
	}
	if tokenInBody.MatchString(readBody(t, resp)) {
		t.Fatal("failed mint leaked a token")
	}
}

func TestTokenNeverInRedirectsOrLogs(t *testing.T) {
	rig := newRig(t, false)
	rig.login()
	resp := rig.do(http.MethodPost, "/token", url.Values{"csrf": {rig.csrf()}})
	raw := tokenInBody.FindString(readBody(t, resp))
	if raw == "" {
		t.Fatal("no token minted")
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("mint answered a redirect (%q) — token pages must render directly", loc)
	}
	if strings.Contains(rig.log.String(), raw) {
		t.Fatal("raw token in logs")
	}
	// The audit line carries the event, user, and login instead.
	if !strings.Contains(rig.log.String(), "token_minted") {
		t.Error("mint not audited")
	}
}

func TestGitHubExchangeFailure(t *testing.T) {
	rig := newRig(t, false)
	rig.github.exchangeErr = errors.New("bad code")
	resp := rig.do(http.MethodGet, "/login", nil)
	state := stateFrom(t, resp)
	resp = rig.do(http.MethodGet, "/github/callback?state="+url.QueryEscape(state)+"&code=c", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("exchange failure = %d, want 403", resp.StatusCode)
	}
}

func TestDashboardShowsCreatedAndLastUsed(t *testing.T) {
	rig := newRig(t, false)
	rig.login()
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	used := time.Date(2026, 7, 3, 9, 30, 0, 0, time.UTC)
	rig.store.rows["h"] = TokenRow{Hash: "h", GitHubUserID: 42, GitHubLogin: "octocat", Created: created, LastUsed: used}

	body := readBody(t, rig.do(http.MethodGet, "/", nil))
	if !strings.Contains(body, "2026-07-01T12:00:00Z") || !strings.Contains(body, "2026-07-03T09:30:00Z") {
		t.Fatalf("dashboard timestamps missing:\n%s", body)
	}
}

func TestHealthz(t *testing.T) {
	rig := newRig(t, false)
	resp := rig.do(http.MethodGet, "/healthz", nil)
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "ok" {
		t.Fatal("healthz broken")
	}
}

func TestOIDCCallbackDisabled(t *testing.T) {
	rig := newRig(t, false)
	resp := rig.do(http.MethodGet, "/oidc/callback?state=s&code=c", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("oidc callback with OIDC off = %d, want 404", resp.StatusCode)
	}
}

func TestLoginWhenAuthenticatedRedirectsHome(t *testing.T) {
	rig := newRig(t, false)
	rig.login()
	resp := rig.do(http.MethodGet, "/login", nil)
	loc, err := resp.Location()
	if err != nil || loc.Path != "/" {
		t.Fatalf("authenticated /login → %v", loc)
	}
}

