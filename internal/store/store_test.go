package store

import (
	"context"
	"errors"
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

func TestRepositoryInventory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	// SE has two repositories; DK has one of them under different case.
	if err := s.RecordNodeScan(ctx, "se", t0, t0.Add(time.Second), []ScannedRepo{
		{FullName: "alice/Demo", ForgejoID: 7, DefaultBranch: "main", HeadSHA: "aaa", Private: true},
		{FullName: "bob/tools", ForgejoID: 8, Empty: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNodeScan(ctx, "dk", t0, t0.Add(2*time.Second), []ScannedRepo{
		{FullName: "alice/demo", ForgejoID: 3, DefaultBranch: "main", HeadSHA: "bbb"},
	}); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].FullName != "alice/Demo" || len(recs[0].Replicas) != 2 || len(recs[1].Replicas) != 1 {
		t.Fatalf("repositories = %+v", recs)
	}
	if !recs[1].FirstSeenAt.Equal(t0) {
		t.Errorf("first seen = %v, want the start of the finding scan %v", recs[1].FirstSeenAt, t0)
	}
	dk := recs[0].Replicas[0] // ordered by node: dk, se
	if dk.Node != "dk" || !dk.Present || dk.HeadSHA != "bbb" || dk.ForgejoID != 3 {
		t.Fatalf("dk replica = %+v", dk)
	}

	// Next SE scan: bob/tools is gone.
	t1 := t0.Add(time.Minute)
	if err := s.RecordNodeScan(ctx, "se", t1, t1.Add(time.Second), []ScannedRepo{
		{FullName: "alice/Demo", ForgejoID: 7, DefaultBranch: "main", HeadSHA: "aaa"},
	}); err != nil {
		t.Fatal(err)
	}
	tools, err := s.Repository(ctx, recs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if tools.Replicas[0].Present || tools.Replicas[0].LastSeenAt == nil || !tools.Replicas[0].LastSeenAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("deleted repo should be kept as not present, last seen at the first scan: %+v", tools.Replicas[0])
	}

	// A failed scan keeps what was found before.
	if err := s.RecordNodeScanFailure(ctx, "dk", t1, t1.Add(time.Second), errors.New("connection refused")); err != nil {
		t.Fatal(err)
	}
	scans, err := s.NodeScans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(scans) != 2 || scans[0].Node != "dk" || scans[0].OK || scans[0].Error != "connection refused" ||
		scans[0].LastSuccessAt == nil || !scans[0].LastSuccessAt.Equal(t0.Add(2*time.Second)) || scans[1].Repositories != 1 {
		t.Fatalf("scans = %+v", scans)
	}

	// Primary designation.
	demo := recs[0].ID
	if prev, err := s.SetPrimary(ctx, demo, "se"); err != nil || prev != "" {
		t.Fatalf("set primary: prev %q, err %v", prev, err)
	}
	if prev, err := s.SetPrimary(ctx, demo, "dk"); err != nil || prev != "se" {
		t.Fatalf("change primary: prev %q, err %v", prev, err)
	}
	if _, err := s.SetPrimary(ctx, demo, "nope"); err == nil {
		t.Fatal("unknown node accepted as primary")
	}
	if prev, err := s.SetPrimary(ctx, demo, ""); err != nil || prev != "dk" {
		t.Fatalf("clear primary: prev %q, err %v", prev, err)
	}
	if _, err := s.SetPrimary(ctx, "00000000-0000-0000-0000-000000000000", "se"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repo: err %v", err)
	}
	if _, err := s.Repository(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: err %v", err)
	}
}
