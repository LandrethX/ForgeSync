package api

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/auth"
)

func TestHistoryFilters(t *testing.T) {
	f := newFixture("s3cret")
	f.srv.DB.Audit(t.Context(), "sceneid:alice", "repo.set_primary", "alice/demo", map[string]any{"to": "se"})
	f.srv.DB.Audit(t.Context(), "sceneid:bob", "session.sign_in", "10.0.0.1", nil)

	var page historyPage
	rec := f.do(req{path: "/api/v1/history?actor=sceneid:bob", bearer: "s3cret"})
	json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != 200 || len(page.Items) != 1 || page.Items[0].Category != "session" {
		t.Errorf("actor filter = %d %+v", rec.Code, page)
	}
	for _, bad := range []string{
		"category=Repo", "from=yesterday", "to=2026-13-01", "limit=501", "cursor=bad",
		"q=" + strings.Repeat("x", 201),
	} {
		if rec := f.do(req{path: "/api/v1/history?" + bad, bearer: "s3cret"}); rec.Code != 400 {
			t.Errorf("%s = %d, want 400", bad[:min(len(bad), 30)], rec.Code)
		}
	}
	if rec := f.do(req{path: "/api/v1/history?category=repo,session&from=2026-09-18T00:00:00Z", bearer: "s3cret"}); rec.Code != 200 {
		t.Errorf("valid filters = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(req{path: "/api/v1/history/actors", bearer: "s3cret"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "forgesync") {
		t.Errorf("actors = %d %s", rec.Code, rec.Body)
	}
}

func TestHistoryExport(t *testing.T) {
	f := newFixture("s3cret")
	// A target that a spreadsheet would run as a formula.
	f.srv.DB.Audit(t.Context(), "sceneid:alice", "repo.set_primary", "=HYPERLINK(\"http://evil\")", map[string]any{"to": "se"})

	rec := f.do(req{path: "/api/v1/history/export?format=csv", bearer: "s3cret"})
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), `attachment; filename="forgesync-history-`) {
		t.Fatalf("csv export = %d %v", rec.Code, rec.Header())
	}
	// The export audits itself before streaming, so it's part of the file.
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(rows) != 3 || rows[0][0] != "time" || rows[1][3] != "history.exported" {
		t.Fatalf("csv = %v, %v", rows, err)
	}
	if rows[2][3] != "repo.set_primary" || !strings.HasPrefix(rows[2][4], "'=") {
		t.Errorf("formula not neutralised: %q", rows[2][4])
	}

	rec = f.do(req{path: "/api/v1/history/export?format=json", bearer: "s3cret"})
	var events []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil || len(events) != 3 {
		// set_primary plus the two exports, each audited before it streams.
		t.Fatalf("json export = %s, %v", rec.Body, err)
	}
	if rec := f.do(req{path: "/api/v1/history/export?format=xml", bearer: "s3cret"}); rec.Code != 400 {
		t.Errorf("unknown format = %d", rec.Code)
	}

	exports := 0
	for _, e := range f.db.audit {
		if e.Action == "history.exported" && e.Actor == "token" {
			exports++
		}
	}
	if exports != 2 {
		t.Errorf("%d audited exports, want 2", exports)
	}

	g, viewer := sessionAs(t, auth.Viewer)
	if rec := g.do(req{path: "/api/v1/history/export", cookie: viewer}); rec.Code != 403 {
		t.Errorf("viewer export = %d, want 403", rec.Code)
	}
}
