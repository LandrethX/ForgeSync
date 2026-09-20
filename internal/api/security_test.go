package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
)

// The endpoints that change something, and the role each one needs. The
// table is the point: a route added without a requireRole around it shows
// up here as a viewer being allowed to use it.
var writeEndpoints = []struct {
	method, path, body string
	need               auth.Role
}{
	{"POST", "/api/v1/conflicts/1/acknowledge", `{"note":"x"}`, auth.Operator},
	{"POST", "/api/v1/conflicts/1/dismiss", `{"note":"x"}`, auth.Operator},
	{"POST", "/api/v1/conflicts/1/reopen", `{}`, auth.Operator},
	{"POST", "/api/v1/inventory/scan", ``, auth.Operator},
	{"POST", "/api/v1/repositories/11111111-1111-1111-1111-111111111111/replicate", ``, auth.Operator},
	{"PUT", "/api/v1/repositories/11111111-1111-1111-1111-111111111111/primary", `{"node":"se"}`, auth.Administrator},
	{"PUT", "/api/v1/users/1/home", `{"node":"se"}`, auth.Administrator},
	{"POST", "/api/v1/users", `{"username":"x","subject":"s"}`, auth.Administrator},
	{"POST", "/api/v1/users/1/nodes", `{}`, auth.Administrator},
	{"PUT", "/api/v1/leadership", `{"controller":"a"}`, auth.Administrator},
	{"DELETE", "/api/v1/leadership", ``, auth.Administrator},
	{"POST", "/api/v1/accounts", `{"username":"x","password":"a-long-enough-one","role":"viewer"}`, auth.Administrator},
	{"PUT", "/api/v1/accounts/acct-x", `{"role":"viewer"}`, auth.Administrator},
	{"PUT", "/api/v1/accounts/acct-x/password", `{"password":"a-long-enough-one"}`, auth.Administrator},
	{"DELETE", "/api/v1/accounts/acct-x", ``, auth.Administrator},
	{"POST", "/api/v1/nodes", `{"name":"se","url":"https://se.example.org","token":"t"}`, auth.Administrator},
	{"POST", "/api/v1/nodes/check", `{"name":"se","url":"https://se.example.org","token":"t"}`, auth.Administrator},
	{"DELETE", "/api/v1/nodes/se", ``, auth.Administrator},
}

// Reads that are not for everyone who can sign in.
var restrictedReads = []struct {
	path string
	need auth.Role
}{
	{"/api/v1/history", auth.Operator},
	{"/api/v1/history/actors", auth.Operator},
	{"/api/v1/history/export", auth.Operator},
	{"/api/v1/accounts", auth.Administrator},
}

// TestEveryWritePathRefusesTheRolesBelowIt is the access-control matrix:
// for each endpoint, nobody signed in, then every role below the one it
// needs, then the role itself. The first two must be refused and the third
// must get past the check -- what it answers after that is its own affair,
// so this asserts only that it isn't a refusal.
func TestEveryWritePathRefusesTheRolesBelowIt(t *testing.T) {
	for _, e := range writeEndpoints {
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			f := newFixture("s3cret")
			withRepos(f)
			if rec := f.do(req{method: e.method, path: e.path, body: e.body, csrf: true}); rec.Code != http.StatusUnauthorized {
				t.Errorf("not signed in = %d, want 401", rec.Code)
			}
			for role := auth.Viewer; role < e.need; role++ {
				g := newFixture("s3cret")
				withRepos(g)
				cookie := signInAs(t, g, role)
				rec := g.do(req{method: e.method, path: e.path, body: e.body, cookie: cookie, csrf: true})
				if rec.Code != http.StatusForbidden {
					t.Errorf("as %s = %d, want 403", role, rec.Code)
				}
			}
			g := newFixture("s3cret")
			withRepos(g)
			cookie := signInAs(t, g, e.need)
			rec := g.do(req{method: e.method, path: e.path, body: e.body, cookie: cookie, csrf: true})
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("as %s = %d, want the role to be enough", e.need, rec.Code)
			}
		})
	}
}

func TestRestrictedReadsRefuseTheRolesBelowThem(t *testing.T) {
	for _, e := range restrictedReads {
		t.Run(e.path, func(t *testing.T) {
			f := newFixture("s3cret")
			if rec := f.do(req{path: e.path}); rec.Code != http.StatusUnauthorized {
				t.Errorf("not signed in = %d, want 401", rec.Code)
			}
			for role := auth.Viewer; role < e.need; role++ {
				g := newFixture("s3cret")
				cookie := signInAs(t, g, role)
				if rec := g.do(req{path: e.path, cookie: cookie}); rec.Code != http.StatusForbidden {
					t.Errorf("as %s = %d, want 403", role, rec.Code)
				}
			}
		})
	}
}

// A write authenticated by the cookie needs the CSRF header whatever the
// role, since that header is the whole of the defence against another site
// posting on a signed-in person's behalf.
func TestWritesFromACookieNeedTheCSRFHeader(t *testing.T) {
	for _, e := range writeEndpoints {
		f := newFixture("s3cret")
		withRepos(f)
		cookie := signInAs(t, f, auth.Administrator)
		rec := f.do(req{method: e.method, path: e.path, body: e.body, cookie: cookie})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), csrfHeader) {
			t.Errorf("%s %s without the CSRF header = %d %s", e.method, e.path, rec.Code, rec.Body)
		}
	}
}

// Who a request came from, with and without a proxy ForgeSync was told to
// trust. Getting this wrong makes the history name the proxy for every
// sign-in and the limiter count everyone's failures together.
func TestClientAddressBehindAProxy(t *testing.T) {
	prefix := func(s string) netip.Prefix {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		fwd     []string
		want    string
	}{
		{"no proxies configured, the header is ignored", nil, "203.0.113.9:4444", []string{"198.51.100.7"}, "203.0.113.9"},
		{"the connection isn't a trusted proxy", []netip.Prefix{prefix("10.0.0.0/8")}, "203.0.113.9:4444", []string{"198.51.100.7"}, "203.0.113.9"},
		{"one trusted proxy", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444", []string{"198.51.100.7"}, "198.51.100.7"},
		{"a proxy named as a bare address", []netip.Prefix{prefix("10.1.2.3/32")}, "10.1.2.3:4444", []string{"198.51.100.7"}, "198.51.100.7"},
		{"two trusted proxies in the chain", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444",
			[]string{"198.51.100.7, 10.9.9.9"}, "198.51.100.7"},
		{"the client's own claim is not believed", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444",
			[]string{"192.0.2.1, 198.51.100.7"}, "198.51.100.7"},
		{"a header split over two lines", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444",
			[]string{"192.0.2.1", "198.51.100.7"}, "198.51.100.7"},
		{"nothing but proxies", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444", []string{"10.9.9.9"}, "10.1.2.3"},
		{"a header that can't be read", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444", []string{"not-an-address"}, "10.1.2.3"},
		{"no header at all", []netip.Prefix{prefix("10.0.0.0/8")}, "10.1.2.3:4444", nil, "10.1.2.3"},
		{"IPv6 behind a trusted proxy", []netip.Prefix{prefix("::1/128")}, "[::1]:4444", []string{"2001:db8::5"}, "2001:db8::5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{TrustedProxies: tc.trusted}
			r := httptest.NewRequest("POST", "/api/v1/session", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.fwd {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := s.clientAddr(r); got != tc.want {
				t.Errorf("clientAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// And the consequence: one client using up its failed sign-ins doesn't
// lock out another that happens to share the proxy.
func TestSignInLimitIsPerClientBehindAProxy(t *testing.T) {
	f := newFixture("s3cret")
	f.srv.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	f.h = f.srv.Handler()

	attempt := func(client, token string) int {
		r := httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(`{"token":"`+token+`"}`))
		r.RemoteAddr = "10.1.2.3:5555"
		r.Header.Set("X-Forwarded-For", client)
		r.Header.Set(csrfHeader, "1")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < 10; i++ {
		attempt("198.51.100.7", "guess")
	}
	if code := attempt("198.51.100.7", "s3cret"); code != http.StatusTooManyRequests {
		t.Errorf("the client that guessed = %d, want 429", code)
	}
	if code := attempt("198.51.100.8", "s3cret"); code != http.StatusOK {
		t.Errorf("another client behind the same proxy = %d, want 200", code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture("s3cret")
	rec := f.do(req{path: "/healthz"})
	for header, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Security-Policy":      "default-src 'self'",
		"Permissions-Policy":           "camera=()",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
	// No HSTS over plain HTTP: this listener may be the one a reverse
	// proxy talks to, and a browser told that http://host is HTTPS-only
	// can't be told otherwise for a year.
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS over plain HTTP = %q, want none", got)
	}

	r := httptest.NewRequest("GET", "/healthz", nil)
	r.TLS = &tls.ConnectionState{}
	over := httptest.NewRecorder()
	f.h.ServeHTTP(over, r)
	if got := over.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=31536000") {
		t.Errorf("HSTS over TLS = %q", got)
	}
}
