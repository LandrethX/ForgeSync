package api

import (
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/store"
)

const aliceID = "33333333-3333-3333-3333-333333333333"

func TestUsers(t *testing.T) {
	f, viewer := sessionAs(t, auth.Viewer)
	f.db.users = []store.UserRecord{{ID: aliceID, Sub: "sub-a", Login: "alice", HomeNode: "dk", HomeSource: "registration",
		Accounts: []store.UserAccount{{Node: "dk", Login: "alice", Present: true}}}}

	rec := f.do(req{path: "/api/v1/users?q=ALI", cookie: viewer})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"home_node":"dk"`) || !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(req{path: "/api/v1/users?q=bob", cookie: viewer}); !strings.Contains(rec.Body.String(), `"total":0,"items":[]`) {
		t.Errorf("filtered list = %s", rec.Body)
	}
	if rec := f.do(req{path: "/api/v1/users/" + aliceID, cookie: viewer}); rec.Code != 200 {
		t.Errorf("get = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/users/nope", cookie: viewer}); rec.Code != 404 {
		t.Errorf("get unknown = %d", rec.Code)
	}
}

func TestSetUserHomeNeedsAdministrator(t *testing.T) {
	path := "/api/v1/users/" + aliceID + "/home"
	users := []store.UserRecord{{ID: aliceID, Login: "alice", HomeNode: "dk", HomeSource: "registration"}}

	f, op := sessionAs(t, auth.Operator)
	f.db.users = users
	if rec := f.do(req{method: "PUT", path: path, body: `{"node":"se"}`, cookie: op, csrf: true}); rec.Code != 403 {
		t.Errorf("operator = %d, want 403", rec.Code)
	}

	f, admin := sessionAs(t, auth.Administrator)
	f.db.users = append([]store.UserRecord(nil), users...)
	for _, body := range []string{`{"node":"xx"}`, `{"node":""}`, `{}`} {
		if rec := f.do(req{method: "PUT", path: path, body: body, cookie: admin, csrf: true}); rec.Code != 400 {
			t.Errorf("%s = %d, want 400", body, rec.Code)
		}
	}
	rec := f.do(req{method: "PUT", path: path, body: `{"node":"se"}`, cookie: admin, csrf: true})
	if rec.Code != 200 || f.db.users[0].HomeNode != "se" || !strings.Contains(rec.Body.String(), `"previous":"dk"`) {
		t.Fatalf("admin = %d %s", rec.Code, rec.Body)
	}
	f.do(req{method: "PUT", path: path, body: `{"node":"se"}`, cookie: admin, csrf: true}) // no change, no audit
	var n int
	for _, e := range f.db.audit {
		if e.Action == "user.set_home" {
			n++
			if e.Target != "alice" {
				t.Errorf("audit target = %q", e.Target)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d user.set_home audit entries, want 1", n)
	}
}
