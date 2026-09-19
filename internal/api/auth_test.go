package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/store"
)

// signInAs makes a ForgeSync account with that role and signs in with it,
// which is how people sign in now: SceneID says who may use the nodes,
// not who looks after the controllers.
func signInAs(t *testing.T, f *fixture, role auth.Role) *http.Cookie {
	t.Helper()
	username := "u-" + role.String()
	if _, err := f.db.CreateAccount(t.Context(), username, "a-long-enough-one", "", role.String(), "test"); err != nil {
		t.Fatal(err)
	}
	rec := f.do(req{method: "POST", path: "/api/v1/session", csrf: true,
		body: `{"username":"` + username + `","password":"a-long-enough-one"}`})
	if rec.Code != 200 {
		t.Fatalf("sign-in as %s = %d %s", role, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// A role is a role wherever it came from: a viewer can read, and the
// history needs an operator.
func TestRoles(t *testing.T) {
	f := newFixture("")
	viewer := signInAs(t, f, auth.Viewer)
	if rec := f.do(req{path: "/api/v1/nodes", cookie: viewer}); rec.Code != 200 {
		t.Errorf("viewer GET /nodes = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/history", cookie: viewer}); rec.Code != 403 {
		t.Errorf("viewer GET /history = %d, want 403", rec.Code)
	}
	operator := signInAs(t, f, auth.Operator)
	if rec := f.do(req{path: "/api/v1/history", cookie: operator}); rec.Code != 200 {
		t.Errorf("operator GET /history = %d", rec.Code)
	}
}

// The sign-in page offers accounts, and the admin token as break-glass.
// There is no SceneID sign-in to offer: it belongs to the nodes.
func TestSignInMethods(t *testing.T) {
	f := newFixture("s3cret")
	var cfg map[string]bool
	json.Unmarshal(f.do(req{path: "/api/v1/auth/config"}).Body.Bytes(), &cfg)
	if !cfg["token_sign_in"] || len(cfg) != 1 {
		t.Errorf("auth config = %v", cfg)
	}
	// The SceneID endpoints are gone rather than turned off.
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/callback"} {
		if rec := f.do(req{path: path}); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
	// The token still signs in, and is still named as itself afterwards.
	f.signIn(t, "s3cret")
	f.db.mu.Lock()
	last := f.db.audit[len(f.db.audit)-1]
	f.db.mu.Unlock()
	if last.Action != "session.sign_in" || last.Actor != "web-token" {
		t.Errorf("break-glass sign-in audit = %+v", last)
	}
	// The CLI's bearer token keeps working.
	if rec := f.do(req{path: "/api/v1/history", bearer: "s3cret"}); rec.Code != 200 {
		t.Errorf("bearer token = %d", rec.Code)
	}
}

// Signing out forgets the session and says nothing about anywhere else.
func TestSignOut(t *testing.T) {
	f := newFixture("")
	admin := signInAs(t, f, auth.Administrator)
	rec := f.do(req{method: "DELETE", path: "/api/v1/session", cookie: admin, csrf: true})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "logout_url\":\"http") {
		t.Fatalf("sign-out = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(req{path: "/api/v1/nodes", cookie: admin}); rec.Code != 401 {
		t.Errorf("after signing out = %d", rec.Code)
	}
}

var _ = store.Account{}
