package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"scenegit.org/forgesync/internal/health"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

type fakeHealth []health.Status

func (f fakeHealth) Snapshot() []health.Status { return f }

func newServer(token string, db error) http.Handler {
	return (&Server{
		AdminToken: token,
		Nodes: []NodeInfo{
			{Name: "dk", URL: "http://forgejo-dk.test:3002", Site: "DK"},
			{Name: "se", URL: "http://forgejo-se.test:3001", Site: "SE"},
		},
		Health: fakeHealth{{Node: "se", State: health.Healthy, Version: "16.0.5"}},
		DB:     fakeDB{db},
		Log:    slog.New(slog.DiscardHandler),
	}).Handler()
}

func get(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProbes(t *testing.T) {
	if rec := get(newServer("", nil), "/healthz", ""); rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := get(newServer("", nil), "/readyz", ""); rec.Code != 200 {
		t.Errorf("/readyz with database = %d", rec.Code)
	}
	if rec := get(newServer("", errors.New("down")), "/readyz", ""); rec.Code != 503 {
		t.Errorf("/readyz without database = %d", rec.Code)
	}
}

func TestAdminAPIAuth(t *testing.T) {
	h := newServer("s3cret", nil)
	if rec := get(h, "/api/v1/nodes", ""); rec.Code != 401 {
		t.Errorf("no token = %d", rec.Code)
	}
	if rec := get(h, "/api/v1/nodes", "wrong"); rec.Code != 401 {
		t.Errorf("wrong token = %d", rec.Code)
	}
	if rec := get(h, "/api/v1/nodes", "s3cret"); rec.Code != 200 {
		t.Errorf("right token = %d", rec.Code)
	}
	if rec := get(newServer("", nil), "/api/v1/nodes", "anything"); rec.Code != 503 {
		t.Errorf("admin API without a configured token = %d, want 503", rec.Code)
	}
}

func TestListNodes(t *testing.T) {
	rec := get(newServer("s3cret", nil), "/api/v1/nodes", "s3cret")
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing")
	}
	var nodes []Node
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	// Nodes follow the config order; a node the monitor hasn't checked is UNKNOWN.
	if nodes[0].Name != "dk" || nodes[0].State != health.Unknown {
		t.Errorf("dk = %+v", nodes[0])
	}
	if nodes[1].Name != "se" || nodes[1].State != health.Healthy || nodes[1].Version != "16.0.5" || nodes[1].Site != "SE" {
		t.Errorf("se = %+v", nodes[1])
	}
}
