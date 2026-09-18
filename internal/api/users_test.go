package api

import (
	"net/http"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/store"
	"scenegit.org/forgesync/internal/webhook"
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

func TestWebhookRoutes(t *testing.T) {
	f := newFixture("s3cret")
	if rec := f.do(req{path: "/api/v1/webhooks", bearer: "s3cret"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("status with webhooks off = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(req{method: "POST", path: "/api/v1/hooks/forgejo/se", body: "{}"}); rec.Code == 202 {
		t.Error("webhook route exists while webhooks are off")
	}

	var gotNode string
	f.srv.Webhooks = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNode = r.PathValue("node")
		w.WriteHeader(http.StatusAccepted)
	})
	f.srv.WebhookStatus = webhook.NewTracker([]string{"se"})
	f.h = f.srv.Handler()
	// No session or token: the receiver checks Forgejo's signature itself.
	if rec := f.do(req{method: "POST", path: "/api/v1/hooks/forgejo/se", body: "{}"}); rec.Code != 202 || gotNode != "se" {
		t.Errorf("delivery = %d, node %q", rec.Code, gotNode)
	}
	if rec := f.do(req{path: "/api/v1/webhooks"}); rec.Code != 401 {
		t.Errorf("status without signing in = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/webhooks", bearer: "s3cret"}); !strings.Contains(rec.Body.String(), `"enabled":true,"nodes":[{"node":"se"`) {
		t.Errorf("status = %s", rec.Body)
	}
}

func TestRepositoryIssues(t *testing.T) {
	f := newFixture("s3cret")
	withRepos(f)
	f.db.issues = []store.IssueRecord{
		{ID: "a", BaseTitle: "same everywhere", Copies: map[string]store.IssueCopy{"se": {Number: 1}, "dk": {Number: 1}}},
		{ID: "b", BaseTitle: "shifted", Copies: map[string]store.IssueCopy{"se": {Number: 2}, "dk": {Number: 3}}},
	}
	f.db.comments = []store.CommentRecord{{ID: "c", IssueID: "b"}}
	rec := f.do(req{path: "/api/v1/repositories/" + demoID + "/issues", bearer: "s3cret"})
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"title":"same everywhere"`) || strings.Count(body, `"numbers_differ":true`) != 1 ||
		!strings.Contains(body, `"comments":1,"numbers_differ":true`) {
		t.Errorf("issues = %d %s", rec.Code, body)
	}
	if rec := f.do(req{path: "/api/v1/repositories/nope/issues", bearer: "s3cret"}); rec.Code != 404 {
		t.Errorf("unknown repository = %d", rec.Code)
	}
}
