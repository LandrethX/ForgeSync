package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/api/v2/whatever"); rec.Code != 404 {
		t.Errorf("/api path = %d, want 404", rec.Code)
	}
	if rec := get("/assets/missing.js"); rec.Code != 404 {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}

	rec := get("/nodes/se")
	if _, err := fs.Stat(dist, "dist/index.html"); err != nil {
		// Built without the UI: explain instead of serving a blank page.
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "make web") {
			t.Errorf("without a build: %d %q", rec.Code, rec.Body)
		}
		return
	}
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Errorf("client route should serve index.html uncached: %d %v", rec.Code, rec.Header())
	}
}
