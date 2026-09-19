package api

import (
	"context"
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

// fakeProvisioner stands in for the replication engine.
type fakeProvisioner struct {
	calls   []string
	created []string
	refused map[string]string
	err     error
}

func (f *fakeProvisioner) CreateUser(_ context.Context, login, subject, fullName, email, home string) ([]string, map[string]string, error) {
	f.calls = append(f.calls, "create "+login+" "+subject+" home="+home)
	return f.created, f.refused, f.err
}

func (f *fakeProvisioner) ProvisionUser(_ context.Context, login string) ([]string, map[string]string, error) {
	f.calls = append(f.calls, "provision "+login)
	return f.created, f.refused, f.err
}

// Adding a user is an Administrator's doing, and it creates a SceneID
// account on the nodes: the subject is what links it to the person.
func TestAddingAUserIsForAdministrators(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	prov := &fakeProvisioner{created: []string{"se", "dk"}, refused: map[string]string{"de": "sceneid_source_id isn't configured for de"}}
	f.srv.Users = prov

	rec := f.do(req{method: "POST", path: "/api/v1/users", cookie: admin, csrf: true,
		body: `{"login":"dave","subject":"dave-sub","full_name":"Dave","home":"se"}`})
	if rec.Code != 201 {
		t.Fatalf("POST users = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"created":["se","dk"]`) ||
		!strings.Contains(rec.Body.String(), "isn't configured for de") {
		t.Errorf("answer = %s", rec.Body)
	}
	if strings.Join(prov.calls, ";") != "create dave dave-sub home=se" {
		t.Errorf("calls = %v", prov.calls)
	}
	// Written down, and the list is rebuilt from a scan.
	if !audited(f, "user.created") {
		t.Error("not audited")
	}
	if f.srv.Inventory.(*fakeInventory).triggered == 0 {
		t.Error("no scan was asked for, so the new user wouldn't appear")
	}

	// An Operator can't.
	g, op := sessionAs(t, auth.Operator)
	g.srv.Users = &fakeProvisioner{}
	rec = g.do(req{method: "POST", path: "/api/v1/users", cookie: op, csrf: true,
		body: `{"login":"eve","subject":"eve-sub","home":"se"}`})
	if rec.Code != 403 {
		t.Errorf("as operator = %d %s", rec.Code, rec.Body)
	}
}

// audited reports that the action was written to the audit log.
func audited(f *fixture, action string) bool {
	for _, e := range f.db.audit {
		if e.Action == action {
			return true
		}
	}
	return false
}

// The subject is what makes the account theirs, so it's required; and a
// controller that can't reach the nodes says so rather than pretending.
func TestAddingAUserNeedsASubjectAndTokens(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	f.srv.Users = &fakeProvisioner{}
	for _, body := range []string{`{"login":"dave","home":"se"}`, `{"subject":"dave-sub","home":"se"}`,
		`{"login":"dave","subject":"dave-sub","home":"nowhere"}`} {
		rec := f.do(req{method: "POST", path: "/api/v1/users", cookie: admin, csrf: true, body: body})
		if rec.Code != 400 {
			t.Errorf("%s = %d %s", body, rec.Code, rec.Body)
		}
	}
	f.srv.Users = nil
	rec := f.do(req{method: "POST", path: "/api/v1/users", cookie: admin, csrf: true,
		body: `{"login":"dave","subject":"dave-sub","home":"se"}`})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "replication is off") {
		t.Errorf("without replication = %d %s", rec.Code, rec.Body)
	}
}

// Someone who has signed in somewhere can be copied to the nodes that
// haven't got them, without waiting for a repository of theirs.
func TestProvisioningAUserOnTheOtherNodes(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	prov := &fakeProvisioner{created: []string{"dk"}}
	f.srv.Users = prov
	f.db.users = []store.UserRecord{{ID: "44444444-4444-4444-4444-444444444444", Login: "erin", Sub: "erin-sub"}}

	rec := f.do(req{method: "POST", path: "/api/v1/users/44444444-4444-4444-4444-444444444444/nodes",
		cookie: admin, csrf: true})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"created":["dk"]`) {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	if strings.Join(prov.calls, ";") != "provision erin" {
		t.Errorf("calls = %v", prov.calls)
	}
	if !audited(f, "user.provisioned") {
		t.Error("not audited")
	}
	rec = f.do(req{method: "POST", path: "/api/v1/users/99999999-9999-9999-9999-999999999999/nodes",
		cookie: admin, csrf: true})
	if rec.Code != 404 {
		t.Errorf("unknown user = %d", rec.Code)
	}
}
