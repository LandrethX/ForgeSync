package api

import (
	"net/http"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/leader"
	"scenegit.org/forgesync/internal/store"
)

// ForgeSync's own accounts are made by an administrator, live in the
// database both controllers share, and sign in with a password.
func TestAccountsAreMadeAndUsedToSignIn(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)

	rec := f.do(req{method: "POST", path: "/api/v1/accounts", cookie: admin, csrf: true,
		body: `{"username":"khav","password":"a-long-enough-one","full_name":"K","role":"administrator"}`})
	if rec.Code != 201 {
		t.Fatalf("POST accounts = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "a-long-enough-one") || strings.Contains(rec.Body.String(), "password_hash") {
		t.Fatalf("the password came back: %s", rec.Body)
	}
	if !audited(f, "account.created") {
		t.Error("not audited")
	}

	// Signing in with it, through the same endpoint the token uses.
	rec = f.do(req{method: "POST", path: "/api/v1/session", csrf: true,
		body: `{"username":"khav","password":"a-long-enough-one"}`})
	if rec.Code != 200 {
		t.Fatalf("sign-in = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"source":"account"`) ||
		!strings.Contains(rec.Body.String(), `"role":"administrator"`) {
		t.Errorf("session = %s", rec.Body)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	// And that session is an administrator's.
	if rec := f.do(req{path: "/api/v1/accounts", cookie: session}); rec.Code != 200 {
		t.Errorf("as the new account = %d %s", rec.Code, rec.Body)
	}

	// The wrong password, a name that doesn't exist and a disabled
	// account are all refused the same way.
	for _, body := range []string{`{"username":"khav","password":"wrong-one-here"}`,
		`{"username":"nobody","password":"a-long-enough-one"}`} {
		rec := f.do(req{method: "POST", path: "/api/v1/session", csrf: true, body: body})
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), "don't match an account") {
			t.Errorf("%s = %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestAccountsNeedAnAdministratorAndAPassword(t *testing.T) {
	f, op := sessionAs(t, auth.Operator)
	if rec := f.do(req{method: "POST", path: "/api/v1/accounts", cookie: op, csrf: true,
		body: `{"username":"x","password":"a-long-enough-one","role":"viewer"}`}); rec.Code != 403 {
		t.Errorf("as operator = %d", rec.Code)
	}

	g, admin := sessionAs(t, auth.Administrator)
	for _, body := range []string{
		`{"username":"","password":"a-long-enough-one","role":"viewer"}`,
		`{"username":"k","password":"short","role":"viewer"}`,
		`{"username":"k","password":"a-long-enough-one","role":"wizard"}`,
		`{"username":"has space","password":"a-long-enough-one","role":"viewer"}`,
	} {
		if rec := g.do(req{method: "POST", path: "/api/v1/accounts", cookie: admin, csrf: true, body: body}); rec.Code != 400 {
			t.Errorf("%s = %d", body, rec.Code)
		}
	}
	// The same name twice is refused, whatever case it's typed in.
	g.do(req{method: "POST", path: "/api/v1/accounts", cookie: admin, csrf: true,
		body: `{"username":"khav","password":"a-long-enough-one","role":"viewer"}`})
	if rec := g.do(req{method: "POST", path: "/api/v1/accounts", cookie: admin, csrf: true,
		body: `{"username":"KHAV","password":"another-long-one","role":"viewer"}`}); rec.Code != 409 {
		t.Errorf("duplicate = %d %s", rec.Code, rec.Body)
	}
}

// Nobody can take away the last way in: an administrator that would
// leave no other administrator able to sign in is kept.
func TestTheLastAdministratorAccountIsKept(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	f.db.accounts = []store.Account{
		{ID: "a1", Username: "khav", Role: "administrator"},
		{ID: "a2", Username: "bob", Role: "operator"},
	}
	for _, r := range []req{
		{method: "PUT", path: "/api/v1/accounts/a1", body: `{"role":"viewer"}`, cookie: admin, csrf: true},
		{method: "PUT", path: "/api/v1/accounts/a1", body: `{"disabled":true}`, cookie: admin, csrf: true},
		{method: "DELETE", path: "/api/v1/accounts/a1", cookie: admin, csrf: true},
	} {
		if rec := f.do(r); rec.Code != 409 {
			t.Errorf("%s %s = %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	// With a second administrator, the first can go.
	f.db.accounts = append(f.db.accounts, store.Account{ID: "a3", Username: "ann", Role: "administrator"})
	if rec := f.do(req{method: "DELETE", path: "/api/v1/accounts/a1", cookie: admin, csrf: true}); rec.Code != 200 {
		t.Errorf("with another administrator = %d %s", rec.Code, rec.Body)
	}
}

// A standby answers these: the accounts belong to the database, not to a
// controller, so being locked out of one is no reason to be stuck.
func TestAccountsCanBeManagedFromAStandby(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	f.srv.Leader = &fakeLeader{leader.State{Leading: false, Name: "forgesync-a"}}
	if rec := f.do(req{method: "POST", path: "/api/v1/accounts", cookie: admin, csrf: true,
		body: `{"username":"khav","password":"a-long-enough-one","role":"administrator"}`}); rec.Code != 201 {
		t.Fatalf("on a standby = %d %s", rec.Code, rec.Body)
	}
	// While something that changes the installation is still refused.
	if rec := f.do(req{method: "POST", path: "/api/v1/inventory/scan", cookie: admin, csrf: true}); rec.Code != 409 {
		t.Errorf("a scan on a standby = %d", rec.Code)
	}
}
