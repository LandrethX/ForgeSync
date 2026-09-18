package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/store"
)

type fakeInventory struct {
	running   bool
	triggered int
}

func (f *fakeInventory) Trigger() bool {
	if f.running {
		return false
	}
	f.triggered++
	return true
}
func (f *fakeInventory) Running() bool           { return f.running }
func (f *fakeInventory) Interval() time.Duration { return 5 * time.Minute }

const (
	demoID  = "11111111-1111-1111-1111-111111111111"
	toolsID = "22222222-2222-2222-2222-222222222222"
)

func withRepos(f *fixture) {
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	later := t0.Add(time.Hour)
	f.db.scans = []store.NodeScan{{Node: "dk", OK: true, LastSuccessAt: &later}, {Node: "se", OK: true, LastSuccessAt: &later}}
	f.db.repos = []store.RepositoryRecord{
		{ID: demoID, FullName: "alice/demo", FirstSeenAt: t0, Replicas: []store.Replica{
			{Node: "dk", Present: true, DefaultBranch: "main", HeadSHA: "aaa"},
			{Node: "se", Present: true, DefaultBranch: "main", HeadSHA: "aaa"}}},
		{ID: toolsID, FullName: "bob/tools", FirstSeenAt: t0, Replicas: []store.Replica{
			{Node: "se", Present: true, DefaultBranch: "main", HeadSHA: "bbb"}}},
	}
}

// sessionAs signs in through the fake SceneID with the given role.
func sessionAs(t *testing.T, role auth.Role) (*fixture, *http.Cookie) {
	t.Helper()
	f, o := newOIDCFixture("s3cret")
	withRepos(f)
	o.finishID = auth.Identity{Subject: "x", Username: "u-" + role.String(), Role: role, Source: "sceneid"}
	for _, c := range signInWithSceneID(t, f, "/").Cookies() {
		if c.Name == sessionCookie {
			return f, c
		}
	}
	t.Fatal("no session")
	return nil, nil
}

func TestListRepositories(t *testing.T) {
	f := newFixture("s3cret")
	withRepos(f)
	var res struct {
		Total  int
		Counts map[string]int
		Items  []struct {
			FullName string `json:"full_name"`
			Status   string
			Nodes    []struct{ Node, Presence string }
		}
	}
	json.Unmarshal(f.do(req{path: "/api/v1/repositories", bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 2 || res.Counts["same"] != 1 || res.Counts["missing"] != 1 || len(res.Items) != 2 {
		t.Fatalf("list = %+v", res)
	}
	if res.Items[1].Status != "missing" || res.Items[1].Nodes[0].Node != "dk" || res.Items[1].Nodes[0].Presence != "absent" {
		t.Errorf("bob/tools = %+v", res.Items[1])
	}

	json.Unmarshal(f.do(req{path: "/api/v1/repositories?status=missing", bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 1 || res.Items[0].FullName != "bob/tools" || res.Counts["same"] != 1 {
		t.Errorf("status filter = %+v (counts ignore filters)", res)
	}
	json.Unmarshal(f.do(req{path: "/api/v1/repositories?q=DEMO", bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 1 || res.Items[0].FullName != "alice/demo" {
		t.Errorf("search = %+v", res)
	}
	json.Unmarshal(f.do(req{path: "/api/v1/repositories?limit=1&offset=1", bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 2 || len(res.Items) != 1 || res.Items[0].FullName != "bob/tools" {
		t.Errorf("paging = %+v", res)
	}
	if rec := f.do(req{path: "/api/v1/repositories?status=bogus", bearer: "s3cret"}); rec.Code != 400 {
		t.Errorf("bad status filter = %d", rec.Code)
	}

	if rec := f.do(req{path: "/api/v1/repositories/" + demoID, bearer: "s3cret"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"same"`) {
		t.Errorf("get = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(req{path: "/api/v1/repositories/nope", bearer: "s3cret"}); rec.Code != 404 {
		t.Errorf("get unknown = %d", rec.Code)
	}
}

func TestSetPrimaryNeedsAdministrator(t *testing.T) {
	body := `{"node":"se"}`
	path := "/api/v1/repositories/" + demoID + "/primary"

	f, op := sessionAs(t, auth.Operator)
	if rec := f.do(req{method: "PUT", path: path, body: body, cookie: op, csrf: true}); rec.Code != 403 {
		t.Errorf("operator = %d, want 403", rec.Code)
	}

	f, admin := sessionAs(t, auth.Administrator)
	if rec := f.do(req{method: "PUT", path: path, body: body, cookie: admin}); rec.Code != 403 {
		t.Errorf("admin without CSRF header = %d, want 403", rec.Code)
	}
	if rec := f.do(req{method: "PUT", path: path, body: `{"node":"xx"}`, cookie: admin, csrf: true}); rec.Code != 400 {
		t.Errorf("unknown node = %d, want 400", rec.Code)
	}
	if rec := f.do(req{method: "PUT", path: path, body: `{}`, cookie: admin, csrf: true}); rec.Code != 400 {
		t.Errorf("missing node field = %d, want 400", rec.Code)
	}
	rec := f.do(req{method: "PUT", path: path, body: body, cookie: admin, csrf: true})
	if rec.Code != 200 || f.db.repos[0].PrimaryNode != "se" {
		t.Fatalf("admin = %d %s", rec.Code, rec.Body)
	}
	// Setting the same value again doesn't add an audit entry.
	f.do(req{method: "PUT", path: path, body: body, cookie: admin, csrf: true})
	f.do(req{method: "PUT", path: path, body: `{"node":""}`, cookie: admin, csrf: true})

	var primary []string
	for _, e := range f.db.audit {
		if e.Action == "repo.set_primary" {
			primary = append(primary, e.Actor+" "+e.Target)
		}
	}
	if len(primary) != 2 || primary[0] != "sceneid:u-administrator alice/demo" {
		t.Errorf("audit = %v, want set then clear", primary)
	}
}

func TestScanNow(t *testing.T) {
	f, viewer := sessionAs(t, auth.Viewer)
	if rec := f.do(req{method: "POST", path: "/api/v1/inventory/scan", cookie: viewer, csrf: true}); rec.Code != 403 {
		t.Errorf("viewer = %d, want 403", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/inventory", cookie: viewer}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"interval_seconds":300`) {
		t.Errorf("viewer GET inventory = %d %s", rec.Code, rec.Body)
	}

	f, op := sessionAs(t, auth.Operator)
	inv := f.srv.Inventory.(*fakeInventory)
	rec := f.do(req{method: "POST", path: "/api/v1/inventory/scan", cookie: op, csrf: true})
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"queued":true`) || inv.triggered != 1 {
		t.Errorf("operator = %d %s", rec.Code, rec.Body)
	}
	inv.running = true
	rec = f.do(req{method: "POST", path: "/api/v1/inventory/scan", cookie: op, csrf: true})
	if !strings.Contains(rec.Body.String(), `"queued":false`) {
		t.Errorf("while running = %s", rec.Body)
	}
}
