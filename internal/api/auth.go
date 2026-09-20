package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/auth"
)

const (
	sessionCookie = "forgesync_session"
	// csrfHeader must accompany state-changing requests authenticated by the
	// session cookie. Other sites can't set it without a CORS preflight, which
	// this server never allows.
	csrfHeader = "X-ForgeSync-CSRF"
)

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

// tokenSignInAllowed: the admin token can be used in the web UI when one
// is configured. It's the break-glass way in -- and the only way to make
// the first ForgeSync account, there being nobody to make it otherwise.
func (s *Server) tokenSignInAllowed() bool {
	return s.AdminToken != ""
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
			var ok2 bool
			if id, _, ok2 = s.Sessions.Get(r.Context(), c.Value); !ok2 {
				unauthorized(w)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "missing " + csrfHeader + " header"})
				return
			}
		} else {
			if s.AdminToken == "" {
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]string{"message": "admin API disabled: set http.admin_token_file"})
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

// clientAddr is who the request came from, as the history records it and
// as the sign-in limiter counts. The connection's own address is the only
// thing that can't be forged, so it's what counts unless a proxy ForgeSync
// was told to trust is the one connecting: then the address that proxy
// reports is better, because otherwise everyone shares one.
func (s *Server) clientAddr(r *http.Request) string {
	host := remoteHost(r)
	if len(s.TrustedProxies) == 0 {
		return host
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !s.trustedProxy(ip) {
		// Not one of ours: whatever it claims about other addresses is
		// the claim of whoever is connecting.
		return host
	}
	// Read X-Forwarded-For from the right: each trusted proxy appends the
	// address it saw, so the right-most address that isn't a proxy of ours
	// is the client as our own proxies saw it. Anything further left was
	// supplied by the client and proves nothing.
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		chain = append(chain, strings.Split(v, ",")...)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		claimed, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			// A header we can't read is a header we can't trust any of.
			return host
		}
		if claimed = claimed.Unmap(); !s.trustedProxy(claimed) {
			return claimed.String()
		}
	}
	return host
}

// trustedProxy reports whether addr is one of the proxies in the config.
func (s *Server) trustedProxy(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range s.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// remoteHost is the address the connection itself came from.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authConfig tells the sign-in page which methods are available. Public.
//
// Signing in to ForgeSync is a ForgeSync account (the user's decision,
// 2026-09-19): SceneID says who may use the *nodes*, and the people who
// look after the controllers are not the same set. The admin token stays
// as the break-glass way in, and as the only way to make the first
// account.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"token_sign_in": s.tokenSignInAllowed()})
}

// setSessionCookie starts a session and sets the cookie. The session is
// in the database, so it works on either controller.
func (s *Server) setSessionCookie(ctx context.Context, w http.ResponseWriter, id auth.Identity) (time.Time, error) {
	key, expires, err := s.Sessions.Create(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: key, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	return expires, nil
}

// ---------------------------------------------------------------- admin token

// createSession signs in with a password or the admin token. A ForgeSync
// account is the ordinary way; the token is the break-glass one, and the
// only way to make the first account.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "missing " + csrfHeader + " header"})
		return
	}
	addr := s.clientAddr(r)
	if blocked, retry := s.limiter.Blocked(addr); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "too many failed sign-ins; try again later"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "couldn't read the request"})
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(body, &creds); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": `expected JSON {"username": ..., "password": ...} or {"token": ...}`})
		return
	}
	if creds.Username != "" || creds.Password != "" {
		s.signInWithPassword(w, r, addr, creds.Username, creds.Password)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.signInWithToken(w, r, addr, creds.Token)
}

// signInWithPassword signs in one of ForgeSync's own accounts. They're in
// the database both controllers share, so it works on either, including
// while SceneID is unreachable -- which is when it matters.
func (s *Server) signInWithPassword(w http.ResponseWriter, r *http.Request, addr, username, password string) {
	account, ok, err := s.DB.CheckPassword(r.Context(), username, password)
	if err != nil {
		s.serverError(w, "check the password", err)
		return
	}
	if !ok {
		s.limiter.Fail(addr)
		s.audit(r.Context(), "account:"+username, "session.sign_in_failed", addr, map[string]any{"source": "account"})
		// The same answer whether the name, the password or the account
		// itself was the problem.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "that username and password don't match an account"})
		return
	}
	s.limiter.Reset(addr)
	role, err := auth.ParseRole(account.Role)
	if err != nil {
		// A role the database holds that this version doesn't know: no
		// access, rather than guessing at what it might have meant.
		s.Log.Error("account has an unknown role", "account", account.Username, "role", account.Role)
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "this account's role isn't one this controller knows"})
		return
	}
	id := auth.Identity{Subject: account.ID, Username: account.Username, Name: account.FullName,
		Role: role, Source: "account"}
	if id.Name == "" {
		id.Name = account.Username
	}
	expires, err := s.setSessionCookie(r.Context(), w, id)
	if err != nil {
		s.serverError(w, "start the session", err)
		return
	}
	s.audit(r.Context(), id.Actor(), "session.sign_in", addr, map[string]any{"source": "account", "role": account.Role})
	writeJSON(w, http.StatusOK, sessionInfo{Identity: id, ExpiresAt: expires.UTC()})
}

func (s *Server) signInWithToken(w http.ResponseWriter, r *http.Request, addr, token string) {
	if !s.tokenSignInAllowed() {
		writeJSON(w, http.StatusForbidden,
			map[string]string{"message": "this controller has no admin token; sign in with a ForgeSync account"})
		return
	}
	breakGlass := map[string]any{"break_glass": true}
	if !s.tokenValid(token) {
		s.limiter.Fail(addr)
		s.audit(r.Context(), "web-token", "session.sign_in_failed", addr, breakGlass)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid admin token"})
		return
	}
	s.limiter.Reset(addr)
	expires, err := s.setSessionCookie(r.Context(), w, webTokenIdentity)
	if err != nil {
		s.serverError(w, "start the session", err)
		return
	}
	s.audit(r.Context(), webTokenIdentity.Actor(), "session.sign_in", addr, breakGlass)
	writeJSON(w, http.StatusOK, sessionInfo{Identity: webTokenIdentity, ExpiresAt: expires.UTC()})
}

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
	id, expires, ok := s.Sessions.Get(r.Context(), c.Value)
	if !ok {
		unauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, sessionInfo{Identity: id, ExpiresAt: expires.UTC()})
}

// deleteSession signs out. The session is in the database both
// controllers share, so this signs out of both at once.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if id, _, ok := s.Sessions.Get(r.Context(), c.Value); ok {
			s.audit(r.Context(), id.Actor(), "session.sign_out", s.clientAddr(r), nil)
		}
		s.Sessions.Delete(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{})
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
	return s.Sessions.Valid(r.Context(), c.Value)
}

func (s *Server) audit(ctx context.Context, actor, action, target string, details map[string]any) {
	if err := s.DB.Audit(ctx, actor, action, target, details); err != nil {
		s.Log.Error("writing audit log failed", "action", action, "error", err)
	}
}
