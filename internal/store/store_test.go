package store

import (
	"context"
	"os"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/health"
)

// These tests need a disposable PostgreSQL database; they drop and recreate
// the public schema. Run them with
//
//	FORGESYNC_TEST_DATABASE_URL=postgres://... go test ./internal/store/
//
// The test environment's postgres service works (see deploy/test/README.md).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("FORGESYNC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FORGESYNC_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 {
		t.Fatal("no migrations applied to an empty database")
	}
	again, err := s.Migrate(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run applied %v, err %v", again, err)
	}
}

func TestNodesStatusAndAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://old", Site: "SE"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://forgejo-se.test:3001", Site: "SE"}}); err != nil {
		t.Fatal(err)
	}
	var url string
	if err := s.pool.QueryRow(ctx, `SELECT url FROM nodes WHERE name = 'se'`).Scan(&url); err != nil || url != "http://forgejo-se.test:3001" {
		t.Fatalf("url = %q, err %v", url, err)
	}

	now := time.Now().UTC()
	st := health.Status{Node: "se", State: health.Healthy, Version: "16.0.5", LastChecked: now, LastSeen: &now}
	for _, step := range []struct {
		state health.State
		prev  health.State
	}{{health.Healthy, health.Unknown}, {health.Healthy, health.Healthy}, {health.Suspect, health.Healthy}} {
		st.State = step.state
		if err := s.RecordNodeStatus(ctx, st, step.prev); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := s.Transitions(ctx, "se")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 2 || tr[0].To != health.Healthy || tr[1].From != health.Healthy || tr[1].To != health.Suspect {
		t.Fatalf("transitions = %+v (unchanged states must not add rows)", tr)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM node_status WHERE node = 'se'`).Scan(&state); err != nil || state != "SUSPECT" {
		t.Fatalf("current state = %q, err %v", state, err)
	}

	if err := s.Audit(ctx, "cli:admin", "node.register", "se", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, "cli:admin", "repo.set_primary", "alice/demo", map[string]any{"primary": "dk"}); err != nil {
		t.Fatal(err)
	}
	var primary string
	if err := s.pool.QueryRow(ctx, `SELECT details->>'primary' FROM audit_log WHERE action = 'repo.set_primary'`).Scan(&primary); err != nil || primary != "dk" {
		t.Fatalf("audit details primary = %q, err %v", primary, err)
	}
}
