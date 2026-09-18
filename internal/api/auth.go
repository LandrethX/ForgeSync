package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/auth"
)

const (
	sessionCookie = "forgesync_session"
	// loginCookie binds an OIDC sign-in attempt to the browser that started
	// it. It must be SameSite=Lax: the callback is a top-level navigation
	// coming back from SceneID, and Strict cookies aren't sent on those.
	loginCookie = "forgesync_login"
	// csrfHeader must accompany state-changing requests authenticated by the
	// session cookie. Other sites can't set it without a CORS preflight, which
	// this server never allows.
	csrfHeader = "X-ForgeSync-CSRF"
)

// OIDCFlow is SceneID sign-in (auth.OIDC).
type OIDCFlow interface {
	Start(ctx context.Context, returnTo string) (authURL, state string, err error)
	Finish(ctx context.Context, state, code string) (auth.Identity, string, string, error)
	LogoutURL(idToken string) string
}

// Identities used for the admin token.
var (
	tokenIdentity    = auth.Identity{Subject: "admin-token", Username: "admin-token", Name: "Admin token", Role: auth.Administrator, Source: "token"}
	webTokenIdentity = auth.Identity{Subject: "admin-token", Username: "admin-token", Name: "Admin token", Role: auth.Administrator, Source: "web-token"}
)

type ctxKey struct{}

// identity returns who made the request.
func identity(r *http.Request) auth.Identity {
	id, _ := r.Context().Value(ctxKey{}).(auth.Identity)
	return id
}

// tokenSignInAllowed: the admin token works in the web UI when SceneID
// sign-in is off, or when it's explicitly kept as a break-glass option.
func (s *Server) tokenSignInAllowed() bool {
	return s.AdminToken != "" && (s.OIDC == nil || s.AllowTokenSignIn)
}

// authenticate accepts the admin token as a bearer token (CLI) or a session
// cookie (web UI), and puts the identity in the request context.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id auth.Identity
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if !s.tokenValid(token) {
				unauthorized(w)
				return
			}
			id = tokenIdentity
		} else if c, err := r.Cookie(sessionCookie); err == nil {
			var ok bool
			if id, _, _, ok = s.Sessions.Get(c.Value); !ok {
				unauthorized(w)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "missing " + csrfHeader + " header"})
				return
			}
		} else {
			if s.AdminToken == "" && s.OIDC == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "admin API disabled: set http.admin_token_file or oidc"})
				return
			}
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// requireRole rejects requests whose identity is below min.
func requireRole(min auth.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if identity(r).Role < min {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "this needs the " + min.String() + " role"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) tokenValid(token string) bool {
	return s.AdminToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminToken)) == 1
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="forgesync"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "not signed in"})
}

func clientAddr(r *http.Request) string {
	// RemoteAddr only: forwarded headers are client-controlled unless a
	// trusted proxy is configured, which this server doesn't support yet.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authConfig tells the sign-in page which methods are available. Public.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"sceneid":       s.OIDC != nil,
		"token_sign_in": s.tokenSignInAllowed(),
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, id auth.Identity, idToken string) time.Time {
	key, expires := s.Sessions.Create(id, idToken)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: key, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	return expires
}

// ---------------------------------------------------------------- admin token

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if !s.tokenSignInAllowed() {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "admin-token sign-in is turned off; sign in with SceneID"})
		return
	}
	if r.Header.Get(csrfHeader) == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "missing " + csrfHeader + " header"})
		return
	}
	addr := clientAddr(r)
	if blocked, retry := s.limiter.Blocked(addr); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "too many failed sign-ins; try again later"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "expected JSON {\"token\": ...}"})
		return
	}
	breakGlass := map[string]any{"break_glass": s.OIDC != nil}
	if !s.tokenValid(body.Token) {
		s.limiter.Fail(addr)
		s.audit(r.Context(), "web-token", "session.sign_in_failed", addr, breakGlass)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid admin token"})
		return
	}
	s.limiter.Reset(addr)
	expires := s.setSessionCookie(w, webTokenIdentity, "")
	s.audit(r.Context(), webTokenIdentity.Actor(), "session.sign_in", addr, breakGlass)
	writeJSON(w, http.StatusOK, sessionInfo{Identity: webTokenIdentity, ExpiresAt: expires.UTC()})
}

// ---------------------------------------------------------------- SceneID

// signInError values end up in /?signin_error=... for the UI to explain.
const (
	errNoRole      = "no_role"
	errExpired     = "expired"
	errCancelled   = "cancelled"
	errUnavailable = "unavailable"
	errFailed      = "failed"
)

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "SceneID sign-in isn't configured"})
		return
	}
	authURL, state, err := s.OIDC.Start(r.Context(), safeReturnTo(r.URL.Query().Get("return_to")))
	if err != nil {
		s.Log.Error("starting SceneID sign-in failed", "error", err)
		http.Redirect(w, r, "/?signin_error="+errUnavailable, http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: state, Path: "/api/v1/auth", MaxAge: 600,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "SceneID sign-in isn't configured"})
		return
	}
	fail := func(code string) { http.Redirect(w, r, "/?signin_error="+code, http.StatusFound) }
	// The attempt is single use either way.
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: "", Path: "/api/v1/auth", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode})

	q := r.URL.Query()
	c, err := r.Cookie(loginCookie)
	// The state in the URL must be the one this browser started with;
	// otherwise someone could make a victim sign in as the attacker.
	if err != nil || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(q.Get("state"))) != 1 {
		fail(errExpired)
		return
	}
	if e := q.Get("error"); e != "" {
		s.Log.Info("SceneID sign-in not completed", "error", e, "description", q.Get("error_description"))
		fail(errCancelled)
		return
	}

	id, idToken, returnTo, err := s.OIDC.Finish(r.Context(), q.Get("state"), q.Get("code"))
	switch {
	case errors.Is(err, auth.ErrNoRole):
		s.audit(r.Context(), id.Actor(), "session.sign_in_denied", clientAddr(r),
			map[string]any{"sub": id.Subject, "reason": "no ForgeSync role"})
		fail(errNoRole)
		return
	case errors.Is(err, auth.ErrUnknownState):
		fail(errExpired)
		return
	case err != nil:
		s.Log.Error("SceneID sign-in failed", "error", err)
		fail(errFailed)
		return
	}
	s.setSessionCookie(w, id, idToken)
	s.audit(r.Context(), id.Actor(), "session.sign_in", clientAddr(r),
		map[string]any{"sub": id.Subject, "role": id.Role.String()})
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// safeReturnTo keeps post-sign-in redirects on this site: a local path, not
// "//host" or "/\\host" (which browsers treat as another host), not the API.
func safeReturnTo(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") ||
		strings.HasPrefix(p, "/api/") {
		return "/"
	}
	if u, err := url.Parse(p); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return p
}

// ---------------------------------------------------------------- session

type sessionInfo struct {
	auth.Identity
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		unauthorized(w)
		return
	}
	id, _, expires, ok := s.Sessions.Get(c.Value)
	if !ok {
		unauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, sessionInfo{Identity: id, ExpiresAt: expires.UTC()})
}

// deleteSession signs out. For SceneID sessions it also returns the SceneID
// sign-out URL, so the browser can end the single sign-on session too.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	var logoutURL string
	if c, err := r.Cookie(sessionCookie); err == nil {
		if id, idToken, _, ok := s.Sessions.Get(c.Value); ok {
			s.audit(r.Context(), id.Actor(), "session.sign_out", clientAddr(r), nil)
			if id.Source == "sceneid" && s.OIDC != nil {
				logoutURL = s.OIDC.LogoutURL(idToken)
			}
		}
		s.Sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{"logout_url": logoutURL})
}

// stillSignedIn re-checks a cookie session during a long-lived stream.
func (s *Server) stillSignedIn(r *http.Request) bool {
	if identity(r).Source == "token" {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.Sessions.Valid(c.Value)
}

func (s *Server) audit(ctx context.Context, actor, action, target string, details map[string]any) {
	if err := s.DB.Audit(ctx, actor, action, target, details); err != nil {
		s.Log.Error("writing audit log failed", "action", action, "error", err)
	}
}
