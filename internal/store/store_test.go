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
	tr, err := s.Transitions(ctx, "se", 10)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first; an unchanged state must not add a row.
	if len(tr) != 2 || tr[0].From != health.Healthy || tr[0].To != health.Suspect || tr[1].To != health.Healthy {
		t.Fatalf("transitions = %+v", tr)
	}
	if all, err := s.Transitions(ctx, "", 1); err != nil || len(all) != 1 || all[0].To != health.Suspect {
		t.Fatalf("all nodes, limit 1 = %+v, err %v", all, err)
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
	entries, err := s.AuditEntries(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != "repo.set_primary" || entries[0].Details["primary"] != "dk" || entries[1].Details == nil {
		t.Fatalf("audit entries = %+v", entries)
	}
	older, err := s.AuditEntries(ctx, 10, entries[0].ID)
	if err != nil || len(older) != 1 || older[0].Action != "node.register" {
		t.Fatalf("entries before %d = %+v, err %v", entries[0].ID, older, err)
	}
}
