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
	if rec.Code != 200 || f.db.conflicts[0].AcknowledgedBy != "account:u-operator" || f.db.conflicts[0].Note != "Fixing it in Git with Bob" {
		t.Fatalf("operator = %d %s / %+v", rec.Code, rec.Body, f.db.conflicts[0])
	}
	var found bool
	for _, e := range f.db.audit {
		if e.Action == "conflict.acknowledged" && e.Target == "alice/demo" && e.Actor == "account:u-operator" {
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

// A conflict ForgeSync can't settle -- a secret it can't copy -- can be
// dismissed: kept, with who and why, but not counted any more.
func TestDismissingAConflict(t *testing.T) {
	f, op := sessionAs(t, auth.Operator)
	f.db.conflicts = []store.Conflict{{ID: 1, RepositoryID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo",
		Kind: "actions_secret_missing", Ref: "TOKEN", State: "open"}}

	rec := f.do(req{method: "POST", path: "/api/v1/conflicts/1/dismiss", cookie: op, csrf: true,
		body: `{"note":"the secret is set by hand on each node"}`})
	if rec.Code != 200 {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body)
	}
	if f.db.conflicts[0].State != "dismissed" || f.db.conflicts[0].DismissedBy != "account:u-operator" {
		t.Fatalf("conflict = %+v", f.db.conflicts[0])
	}
	if !strings.Contains(rec.Body.String(), "set by hand") {
		t.Errorf("the note wasn't kept: %s", rec.Body)
	}
	if !audited(f, "conflict.dismissed") {
		t.Error("not audited")
	}
	// It doesn't count any more.
	if n, _ := f.db.OpenConflicts(t.Context()); n != 0 {
		t.Errorf("open conflicts = %d", n)
	}
	// Dismissing it twice is refused rather than silently doing nothing.
	if rec := f.do(req{method: "POST", path: "/api/v1/conflicts/1/dismiss", cookie: op, csrf: true, body: `{}`}); rec.Code != 409 {
		t.Errorf("dismissing a dismissed one = %d %s", rec.Code, rec.Body)
	}

	// And it can be brought back.
	rec = f.do(req{method: "POST", path: "/api/v1/conflicts/1/reopen", cookie: op, csrf: true})
	if rec.Code != 200 || f.db.conflicts[0].State != "open" {
		t.Fatalf("reopen = %d %s / %+v", rec.Code, rec.Body, f.db.conflicts[0])
	}
	if !audited(f, "conflict.reopened") {
		t.Error("the reopening wasn't audited")
	}
}

func TestDismissingNeedsAnOperator(t *testing.T) {
	f, viewer := sessionAs(t, auth.Viewer)
	f.db.conflicts = []store.Conflict{{ID: 1, RepositoryID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", State: "open"}}
	if rec := f.do(req{method: "POST", path: "/api/v1/conflicts/1/dismiss", cookie: viewer, csrf: true, body: `{}`}); rec.Code != 403 {
		t.Errorf("as viewer = %d", rec.Code)
	}
	g, op := sessionAs(t, auth.Operator)
	if rec := g.do(req{method: "POST", path: "/api/v1/conflicts/99/dismiss", cookie: op, csrf: true, body: `{}`}); rec.Code != 404 {
		t.Errorf("unknown conflict = %d", rec.Code)
	}
}
