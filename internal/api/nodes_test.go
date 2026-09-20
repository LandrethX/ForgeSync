package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/nodes"
)

// A NodeAdmin that answers however the test needs.
type fakeNodeAdmin struct {
	report nodes.Report
	err    error
	added  []nodes.NewNode
	by     []string
}

func (f *fakeNodeAdmin) Check(context.Context, nodes.NewNode) (nodes.Report, error) {
	return f.report, f.err
}
func (f *fakeNodeAdmin) Add(_ context.Context, n nodes.NewNode, by string) (nodes.Report, error) {
	if f.err != nil {
		return nodes.Report{}, f.err
	}
	if f.report.OK {
		f.added = append(f.added, n)
		f.by = append(f.by, by)
	}
	return f.report, nil
}

const goodNode = `{"name":"se","url":"https://forgejo-se.example.org","site":"SE","token":"a-token","sceneid_source_id":2}`

func TestAddingANodeStoresItAndSaysWhoDidIt(t *testing.T) {
	f := newFixture("s3cret")
	admin := &fakeNodeAdmin{report: nodes.Report{OK: true, ServiceUser: "forgesync", Version: "16.0.1"}}
	f.srv.NodeAdmin = admin
	cookie := signInAs(t, f, auth.Administrator)

	rec := f.do(req{method: "POST", path: "/api/v1/nodes", body: goodNode, cookie: cookie, csrf: true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /nodes = %d %s", rec.Code, rec.Body)
	}
	if len(admin.added) != 1 || admin.added[0].Name != "se" {
		t.Fatalf("stored %+v", admin.added)
	}
	if len(admin.by) != 1 || admin.by[0] == "" {
		t.Errorf("nobody was recorded as having added it: %v", admin.by)
	}
	var sawAudit bool
	for _, e := range f.db.audit {
		if e.Action == "node.added" && e.Target == "se" {
			sawAudit = true
			if _, leaked := e.Details["token"]; leaked {
				t.Error("the token was written to the history")
			}
		}
	}
	if !sawAudit {
		t.Error("adding a node was not audited")
	}
}

// A node ForgeSync cannot use is answered with the reasons and is not
// stored: the report is the point, so it has to come back.
func TestANodeThatFailsItsChecksIsRefusedWithTheReasons(t *testing.T) {
	f := newFixture("s3cret")
	admin := &fakeNodeAdmin{report: nodes.Report{OK: false, Findings: []nodes.Finding{
		{Check: "That account is a site admin", OK: false, Blocking: true, Detail: "make it an administrator"},
	}}}
	f.srv.NodeAdmin = admin
	cookie := signInAs(t, f, auth.Administrator)

	rec := f.do(req{method: "POST", path: "/api/v1/nodes", body: goodNode, cookie: cookie, csrf: true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /nodes = %d, want 422", rec.Code)
	}
	if len(admin.added) != 0 {
		t.Fatal("it was stored anyway")
	}
	if !strings.Contains(rec.Body.String(), "site admin") {
		t.Errorf("the reason did not come back: %s", rec.Body)
	}
	for _, e := range f.db.audit {
		if e.Action == "node.added" {
			t.Error("a node that was refused was audited as added")
		}
	}
}

// Checking writes nothing, whatever it finds.
func TestCheckingANodeWritesNothing(t *testing.T) {
	f := newFixture("s3cret")
	admin := &fakeNodeAdmin{report: nodes.Report{OK: true, ServiceUser: "forgesync"}}
	f.srv.NodeAdmin = admin
	cookie := signInAs(t, f, auth.Administrator)

	rec := f.do(req{method: "POST", path: "/api/v1/nodes/check", body: goodNode, cookie: cookie, csrf: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /nodes/check = %d %s", rec.Code, rec.Body)
	}
	if len(admin.added) != 0 {
		t.Fatal("checking stored the node")
	}
	for _, e := range f.db.audit {
		if e.Action == "node.added" {
			t.Error("checking was audited as adding")
		}
	}
}

// Without a node key there is nowhere safe to put the token, and saying
// so beats half working.
func TestWithoutANodeKeyTheEndpointSaysSo(t *testing.T) {
	f := newFixture("s3cret")
	f.srv.NodeAdmin = nil
	cookie := signInAs(t, f, auth.Administrator)
	rec := f.do(req{method: "POST", path: "/api/v1/nodes", body: goodNode, cookie: cookie, csrf: true})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /nodes = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "node_key_file") {
		t.Errorf("it did not say what to set: %s", rec.Body)
	}
}

func TestRetiringANodeIsAudited(t *testing.T) {
	f := newFixture("s3cret")
	cookie := signInAs(t, f, auth.Administrator)
	rec := f.do(req{method: "DELETE", path: "/api/v1/nodes/dk", cookie: cookie, csrf: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /nodes/dk = %d %s", rec.Code, rec.Body)
	}
	if len(f.db.retired) != 1 || f.db.retired[0] != "dk" {
		t.Fatalf("retired %v", f.db.retired)
	}
	var sawAudit bool
	for _, e := range f.db.audit {
		if e.Action == "node.retired" && e.Target == "dk" {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Error("retiring a node was not audited")
	}
	// Retiring one that is not there is a 404, not a quiet success.
	if rec := f.do(req{method: "DELETE", path: "/api/v1/nodes/dk", cookie: cookie, csrf: true}); rec.Code != http.StatusNotFound {
		t.Errorf("retiring it twice = %d, want 404", rec.Code)
	}
}
