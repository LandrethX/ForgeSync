package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
)

// fakeOIDC stands in for auth.OIDC: Start hands out a fixed state and
// Finish returns whatever the test sets.
type fakeOIDC struct {
	startErr  error
	returnTo  string
	finishID  auth.Identity
	finishErr error
	finished  []string // states passed to Finish
}

func (f *fakeOIDC) Start(_ context.Context, returnTo string) (string, string, error) {
	if f.startErr != nil {
		return "", "", f.startErr
	}
	f.returnTo = returnTo
	return "https://sceneid.example/auth?state=st-1", "st-1", nil
}

func (f *fakeOIDC) Finish(_ context.Context, state, code string) (auth.Identity, string, string, error) {
	f.finished = append(f.finished, state)
	return f.finishID, "raw-id-token", f.returnTo, f.finishErr
}

func (f *fakeOIDC) LogoutURL(idToken string) string {
	return "https://sceneid.example/logout?id_token_hint=" + idToken
}

var alice = auth.Identity{Subject: "3f1c", Username: "alice", Name: "Alice", Role: auth.Operator, Source: "sceneid"}

func newOIDCFixture(token string) (*fixture, *fakeOIDC) {
	f := newFixture(token)
	o := &fakeOIDC{finishID: alice}
	f.srv.OIDC = o
	f.h = f.srv.Handler()
	return f, o
}

func cookieNamed(rec interface{ Result() *http.Response }, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// signInWithSceneID runs login + callback and returns the callback response.
func signInWithSceneID(t *testing.T, f *fixture, returnTo string) *http.Response {
	t.Helper()
	rec := f.do(req{path: "/api/v1/auth/login?return_to=" + url.QueryEscape(returnTo)})
	if rec.Code != 302 || rec.Header().Get("Location") != "https://sceneid.example/auth?state=st-1" {
		t.Fatalf("login = %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	lc := cookieNamed(rec, loginCookie)
	if lc == nil || !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode || lc.Path != "/api/v1/auth" || !lc.Secure {
		t.Fatalf("login cookie = %+v", lc)
	}
	return f.do(req{path: "/api/v1/auth/callback?state=st-1&code=abc", cookie: lc}).Result()
}

func TestSceneIDSignIn(t *testing.T) {
	f, _ := newOIDCFixture("s3cret")
	res := signInWithSceneID(t, f, "/nodes/se")
	if res.StatusCode != 302 || res.Header.Get("Location") != "/nodes/se" {
		t.Fatalf("callback = %d, Location %q", res.StatusCode, res.Header.Get("Location"))
	}
	var sc *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			sc = c
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", sc)
	}

	var info struct {
		Username, Role, Source string
	}
	json.Unmarshal(f.do(req{path: "/api/v1/session", cookie: sc}).Body.Bytes(), &info)
	if info.Username != "alice" || info.Role != "operator" || info.Source != "sceneid" {
		t.Errorf("session = %+v", info)
	}

	rec := f.do(req{method: "DELETE", path: "/api/v1/session", cookie: sc})
	if !strings.Contains(rec.Body.String(), "id_token_hint=raw-id-token") {
		t.Errorf("sign out should return the SceneID logout URL, got %s", rec.Body)
	}
	want := []string{"sceneid:alice session.sign_in", "sceneid:alice session.sign_out"}
	if got := f.db.actions(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("audit = %v", got)
	}
}

func TestSceneIDCallbackRejections(t *testing.T) {
	t.Run("no login cookie (someone else's callback link)", func(t *testing.T) {
		f, o := newOIDCFixture("")
		rec := f.do(req{path: "/api/v1/auth/callback?state=st-1&code=abc"})
		if rec.Header().Get("Location") != "/?signin_error=expired" || len(o.finished) != 0 {
			t.Errorf("Location %q, Finish called %d times", rec.Header().Get("Location"), len(o.finished))
		}
	})
	t.Run("state doesn't match the cookie", func(t *testing.T) {
		f, o := newOIDCFixture("")
		rec := f.do(req{path: "/api/v1/auth/callback?state=other&code=abc", cookie: &http.Cookie{Name: loginCookie, Value: "st-1"}})
		if rec.Header().Get("Location") != "/?signin_error=expired" || len(o.finished) != 0 {
			t.Errorf("Location %q, Finish called %d times", rec.Header().Get("Location"), len(o.finished))
		}
	})
	t.Run("user cancelled at SceneID", func(t *testing.T) {
		f, _ := newOIDCFixture("")
		rec := f.do(req{path: "/api/v1/auth/callback?state=st-1&error=access_denied", cookie: &http.Cookie{Name: loginCookie, Value: "st-1"}})
		if rec.Header().Get("Location") != "/?signin_error=cancelled" {
			t.Errorf("Location %q", rec.Header().Get("Location"))
		}
	})
	t.Run("no ForgeSync role", func(t *testing.T) {
		f, o := newOIDCFixture("")
		o.finishErr = auth.ErrNoRole
		o.finishID = auth.Identity{Subject: "9x", Username: "carol", Source: "sceneid"}
		res := signInWithSceneID(t, f, "/")
		if res.Header.Get("Location") != "/?signin_error=no_role" {
			t.Errorf("Location %q", res.Header.Get("Location"))
		}
		for _, c := range res.Cookies() {
			if c.Name == sessionCookie && c.Value != "" {
				t.Error("a session cookie was set for a user without a role")
			}
		}
		if got := f.db.actions(); len(got) != 1 || got[0] != "sceneid:carol session.sign_in_denied" {
			t.Errorf("audit = %v", got)
		}
	})
	t.Run("SceneID unreachable", func(t *testing.T) {
		f, o := newOIDCFixture("")
		o.startErr = errors.New("discovery failed")
		rec := f.do(req{path: "/api/v1/auth/login"})
		if rec.Code != 302 || rec.Header().Get("Location") != "/?signin_error=unavailable" {
			t.Errorf("login = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	})
}

func TestRoles(t *testing.T) {
	f, o := newOIDCFixture("")
	o.finishID = auth.Identity{Subject: "b0b", Username: "bob", Role: auth.Viewer, Source: "sceneid"}
	var viewer *http.Cookie
	for _, c := range signInWithSceneID(t, f, "/").Cookies() {
		if c.Name == sessionCookie {
			viewer = c
		}
	}
	if rec := f.do(req{path: "/api/v1/nodes", cookie: viewer}); rec.Code != 200 {
		t.Errorf("viewer GET /nodes = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/audit", cookie: viewer}); rec.Code != 403 {
		t.Errorf("viewer GET /audit = %d, want 403", rec.Code)
	}

	o.finishID = alice // operator
	var operator *http.Cookie
	for _, c := range signInWithSceneID(t, f, "/").Cookies() {
		if c.Name == sessionCookie {
			operator = c
		}
	}
	if rec := f.do(req{path: "/api/v1/audit", cookie: operator}); rec.Code != 200 {
		t.Errorf("operator GET /audit = %d", rec.Code)
	}
}

func TestTokenSignInWithSceneID(t *testing.T) {
	f, _ := newOIDCFixture("s3cret")
	var cfg map[string]bool
	json.Unmarshal(f.do(req{path: "/api/v1/auth/config"}).Body.Bytes(), &cfg)
	if !cfg["sceneid"] || cfg["token_sign_in"] {
		t.Errorf("auth config = %v", cfg)
	}
	rec := f.do(req{method: "POST", path: "/api/v1/session", body: `{"token":"s3cret"}`, csrf: true})
	if rec.Code != 403 {
		t.Errorf("token sign-in with SceneID on = %d, want 403", rec.Code)
	}
	// The CLI's bearer token keeps working.
	if rec := f.do(req{path: "/api/v1/audit", bearer: "s3cret"}); rec.Code != 200 {
		t.Errorf("bearer token = %d", rec.Code)
	}

	f.srv.AllowTokenSignIn = true
	f.h = f.srv.Handler()
	f.signIn(t, "s3cret")
	f.db.mu.Lock()
	last := f.db.audit[len(f.db.audit)-1]
	f.db.mu.Unlock()
	if last.Action != "session.sign_in" || last.Actor != "web-token" {
		t.Errorf("break-glass sign-in audit = %+v", last)
	}
}

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/nodes/se":            "/nodes/se",
		"/audit?x=1":           "/audit?x=1",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"https://evil.example": "/",
		"nodes":                "/",
		"/api/v1/session":      "/",
	} {
		if got := safeReturnTo(in); got != want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}
