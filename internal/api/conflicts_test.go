package api

import (
	"encoding/json"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/store"
)

func withConflicts(f *fixture) {
	f.db.conflicts = []store.Conflict{
		{ID: 1, RepositoryID: demoID, FullName: "alice/demo", Kind: "git_diverged", Ref: "refs/heads/main", State: "open"},
		{ID: 2, RepositoryID: toolsID, FullName: "bob/tools", Kind: "default_branch_mismatch", State: "cleared"},
	}
}

func TestListConflicts(t *testing.T) {
	f := newFixture("s3cret")
	withConflicts(f)
	var res conflictList
	json.Unmarshal(f.do(req{path: "/api/v1/conflicts", bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 1 || res.Items[0].ID != 1 || res.Counts["cleared"] != 1 {
		t.Errorf("default (open) = %+v", res)
	}
	json.Unmarshal(f.do(req{path: "/api/v1/conflicts?state=all", bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 2 {
		t.Errorf("all = %+v", res)
	}
	json.Unmarshal(f.do(req{path: "/api/v1/conflicts?state=all&repository=" + toolsID, bearer: "s3cret"}).Body.Bytes(), &res)
	if res.Total != 1 || res.Items[0].FullName != "bob/tools" {
		t.Errorf("by repository = %+v", res)
	}
	if rec := f.do(req{path: "/api/v1/conflicts?state=bogus", bearer: "s3cret"}); rec.Code != 400 {
		t.Errorf("bad state = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/conflicts/2", bearer: "s3cret"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "bob/tools") {
		t.Errorf("get = %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/api/v1/conflicts/99", "/api/v1/conflicts/abc", "/api/v1/conflicts/-1"} {
		if rec := f.do(req{path: p, bearer: "s3cret"}); rec.Code != 404 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	var o Overview
	json.Unmarshal(f.do(req{path: "/api/v1/overview", bearer: "s3cret"}).Body.Bytes(), &o)
	if o.OpenConflicts != 1 {
		t.Errorf("overview open conflicts = %d", o.OpenConflicts)
	}
}

func TestAcknowledgeConflict(t *testing.T) {
	path := "/api/v1/conflicts/1/acknowledge"

	f, viewer := sessionAs(t, auth.Viewer)
	withConflicts(f)
	if rec := f.do(req{method: "POST", path: path, body: `{"note":"x"}`, cookie: viewer, csrf: true}); rec.Code != 403 {
		t.Errorf("viewer = %d, want 403", rec.Code)
	}

	f, op := sessionAs(t, auth.Operator)
	withConflicts(f)
	if rec := f.do(req{method: "POST", path: path, body: `{"note":"x"}`, cookie: op}); rec.Code != 403 {
		t.Errorf("without CSRF header = %d, want 403", rec.Code)
	}
	long := strings.Repeat("é", 1001)
	if rec := f.do(req{method: "POST", path: path, body: `{"note":"` + long + `"}`, cookie: op, csrf: true}); rec.Code != 400 {
		t.Errorf("1001-character note = %d, want 400", rec.Code)
	}
	rec := f.do(req{method: "POST", path: path, body: `{"note":"  Fixing it in Git with Bob  "}`, cookie: op, csrf: true})
	if rec.Code != 200 || f.db.conflicts[0].AcknowledgedBy != "sceneid:u-operator" || f.db.conflicts[0].Note != "Fixing it in Git with Bob" {
		t.Fatalf("operator = %d %s / %+v", rec.Code, rec.Body, f.db.conflicts[0])
	}
	var found bool
	for _, e := range f.db.audit {
		if e.Action == "conflict.acknowledged" && e.Target == "alice/demo" && e.Actor == "sceneid:u-operator" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit entry: %v", f.db.actions())
	}
	if rec := f.do(req{method: "POST", path: "/api/v1/conflicts/99/acknowledge", body: `{}`, cookie: op, csrf: true}); rec.Code != 404 {
		t.Errorf("unknown conflict = %d", rec.Code)
	}
}
