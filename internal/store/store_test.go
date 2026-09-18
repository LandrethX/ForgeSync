package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
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
	events, _, err := s.History(ctx, EventFilter{Categories: []string{"repo", "node"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// Two audit entries (repo.set_primary, node.register) and two node state changes.
	if len(events) != 4 || events[0].Action != "repo.set_primary" || events[0].Details["primary"] != "dk" {
		t.Fatalf("history = %+v", events)
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
	if _, err := s.SetPrimary(ctx, demo, ""); err == nil {
		t.Fatal("a primary was cleared")
	}
	if rec, _ := s.Repository(ctx, demo); rec.PrimaryNode != "dk" || rec.PrimarySource != "manual" {
		t.Fatalf("after changes: primary %q source %q, want dk manual", rec.PrimaryNode, rec.PrimarySource)
	}
	if _, err := s.SetPrimary(ctx, "00000000-0000-0000-0000-000000000000", "se"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repo: err %v", err)
	}
	if _, err := s.Repository(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: err %v", err)
	}
}

func TestAssignOriginPrimaries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	nodes := []string{"se", "dk", "de"}
	var recs []NodeRecord
	for _, n := range nodes {
		recs = append(recs, NodeRecord{Name: n, URL: "http://" + n})
	}
	if err := s.SyncNodes(ctx, recs); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	scan := func(node string, repos ...ScannedRepo) {
		t.Helper()
		if err := s.RecordNodeScan(ctx, node, t0, t0.Add(time.Second), repos); err != nil {
			t.Fatal(err)
		}
	}
	scan("se",
		ScannedRepo{FullName: "alice/only-se", Created: t0.Add(-day)},
		ScannedRepo{FullName: "alice/both", Created: t0.Add(-day)},
		ScannedRepo{FullName: "alice/mirrored", Created: t0.Add(-3 * day), Mirror: true},
		ScannedRepo{FullName: "alice/chosen", Created: t0.Add(-day)})
	scan("dk",
		ScannedRepo{FullName: "alice/both", Created: t0.Add(-2 * day)}, // created on DK first
		ScannedRepo{FullName: "alice/mirrored", Created: t0.Add(-2 * day)},
		ScannedRepo{FullName: "alice/chosen", Created: t0.Add(-2 * day)})

	// DE has never been scanned, so it could hide an earlier copy: nothing is assigned.
	if got, ok, err := s.AssignOriginPrimaries(ctx, nodes); err != nil || ok || len(got) != 0 {
		t.Fatalf("with DE unscanned: %v %v %v", got, ok, err)
	}
	scan("de")

	all, _ := s.Repositories(ctx)
	byName := map[string]RepositoryRecord{}
	for _, r := range all {
		byName[r.FullName] = r
	}
	if _, err := s.SetPrimary(ctx, byName["alice/chosen"].ID, "se"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.AssignOriginPrimaries(ctx, nodes)
	if err != nil || !ok {
		t.Fatalf("assign: ok %v err %v", ok, err)
	}
	want := map[string]string{"alice/only-se": "se", "alice/both": "dk", "alice/mirrored": "dk"}
	if len(got) != len(want) {
		t.Fatalf("assigned %+v, want %v", got, want)
	}
	for _, a := range got {
		if want[a.FullName] != a.Node {
			t.Errorf("%s -> %s, want %s", a.FullName, a.Node, want[a.FullName])
		}
	}
	if b := got[0]; b.FullName == "alice/both" && len(b.Nodes) != 2 {
		t.Errorf("alice/both nodes = %v", b.Nodes)
	}
	all, _ = s.Repositories(ctx)
	for _, r := range all {
		source := "origin"
		if r.FullName == "alice/chosen" {
			source = "manual" // an Administrator's choice is never replaced
		}
		if r.PrimarySource != source {
			t.Errorf("%s: primary %s source %q, want %q", r.FullName, r.PrimaryNode, r.PrimarySource, source)
		}
	}
	if again, _, _ := s.AssignOriginPrimaries(ctx, nodes); len(again) != 0 {
		t.Errorf("second run assigned %+v", again)
	}
}

func TestConflicts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if err := s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo"}, {FullName: "bob/tools"}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Repositories(ctx)
	demo, tools := recs[0].ID, recs[1].ID
	diverged := FoundConflict{RepositoryID: demo, Kind: "git_diverged", Ref: "refs/heads/main", Details: map[string]any{"heads": map[string]any{"se": "a", "dk": "b"}}}

	changes, err := s.SyncConflicts(ctx, []FoundConflict{diverged}, []string{demo, tools}, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Change != "opened" || changes[0].FullName != "alice/demo" {
		t.Fatalf("first sync = %+v", changes)
	}
	// Found again: refreshed, not reopened.
	diverged.Details = map[string]any{"heads": map[string]any{"se": "a2", "dk": "b"}}
	if changes, err = s.SyncConflicts(ctx, []FoundConflict{diverged}, []string{demo}, nil, t0.Add(time.Minute)); err != nil || len(changes) != 0 {
		t.Fatalf("second sync = %+v, %v", changes, err)
	}
	// Not concluded for demo this time (not in checked): stays open.
	if changes, err = s.SyncConflicts(ctx, nil, []string{tools}, nil, t0.Add(2*time.Minute)); err != nil || len(changes) != 0 {
		t.Fatalf("unchecked repo = %+v, %v", changes, err)
	}
	items, total, counts, err := s.Conflicts(ctx, ConflictFilter{State: "open", Limit: 10})
	if err != nil || total != 1 || counts["open"] != 1 || items[0].LastSeenAt.Equal(t0) {
		t.Fatalf("open = %+v total %d counts %v err %v", items, total, counts, err)
	}
	heads := items[0].Details["heads"].(map[string]any)
	if heads["se"] != "a2" {
		t.Errorf("details not refreshed: %v", items[0].Details)
	}

	if err := s.AcknowledgeConflict(ctx, items[0].ID, "sceneid:bob", "talking to the team", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeConflict(ctx, 999999, "x", "", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown conflict: %v", err)
	}

	// Checked and not found: cleared.
	changes, err = s.SyncConflicts(ctx, nil, []string{demo}, nil, t0.Add(3*time.Minute))
	if err != nil || len(changes) != 1 || changes[0].Change != "cleared" {
		t.Fatalf("clearing = %+v, %v", changes, err)
	}
	c, err := s.ConflictByID(ctx, items[0].ID)
	if err != nil || c.State != "cleared" || c.ClearedAt == nil || c.AcknowledgedBy != "sceneid:bob" || c.Note != "talking to the team" {
		t.Fatalf("cleared conflict = %+v, %v", c, err)
	}
	// It can come back as a new conflict.
	if changes, _ = s.SyncConflicts(ctx, []FoundConflict{diverged}, []string{demo}, nil, t0.Add(4*time.Minute)); len(changes) != 1 {
		t.Fatalf("reopening = %+v", changes)
	}
	_, total, counts, _ = s.Conflicts(ctx, ConflictFilter{RepositoryID: demo, Limit: 10})
	if total != 2 || counts["open"] != 1 || counts["cleared"] != 1 {
		t.Errorf("per repository: total %d counts %v", total, counts)
	}
	if n, err := s.OpenConflicts(ctx); err != nil || n != 1 {
		t.Errorf("open count = %d, %v", n, err)
	}

	// A check scoped to other kinds doesn't clear this one.
	if changes, _ = s.SyncConflicts(ctx, nil, []string{demo}, []string{"default_branch_mismatch"}, t0.Add(5*time.Minute)); len(changes) != 0 {
		t.Errorf("a scoped check cleared another kind: %+v", changes)
	}
	if changes, _ = s.SyncConflicts(ctx, nil, []string{demo}, []string{"git_diverged"}, t0.Add(6*time.Minute)); len(changes) != 1 {
		t.Errorf("a check for this kind should clear it: %+v", changes)
	}
}

func TestHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		if _, err := s.pool.Exec(ctx, `INSERT INTO audit_log (at, actor, action, target, details) VALUES ($1, $2, $3, $4, $5)`,
			t0.Add(time.Duration(i)*time.Minute), []string{"sceneid:alice", "sceneid:bob"}[i%2],
			[]string{"session.sign_in", "repo.set_primary"}[i%2], "alice/demo_"+strconv.Itoa(i), map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	st := health.Status{Node: "se", State: health.Unreachable, LastChecked: t0.Add(3 * time.Minute), LastError: "connection refused"}
	if err := s.RecordNodeStatus(ctx, st, health.Healthy); err != nil {
		t.Fatal(err)
	}

	all, next, err := s.History(ctx, EventFilter{Limit: 100})
	if err != nil || len(all) != 8 || next != "" {
		t.Fatalf("all = %d entries, next %q, err %v", len(all), next, err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].At.After(all[i-1].At) {
			t.Fatalf("not newest first at %d: %v then %v", i, all[i-1].At, all[i].At)
		}
	}
	var node Event
	for _, e := range all {
		if e.Category == "node" {
			node = e
		}
	}
	if node.Action != "node.state_changed" || node.Target != "se" || node.Details["to"] != "UNREACHABLE" || node.Details["error"] != "connection refused" {
		t.Errorf("node event = %+v", node)
	}

	// Paging with a cursor visits every entry exactly once.
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		page, nextCursor, err := s.History(ctx, EventFilter{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			if seen[e.ID] {
				t.Fatalf("%s returned twice", e.ID)
			}
			seen[e.ID] = true
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
		if pages > 5 {
			t.Fatal("paging doesn't end")
		}
	}
	if len(seen) != 8 {
		t.Errorf("paging saw %d entries, want 8", len(seen))
	}

	count := func(f EventFilter) int {
		t.Helper()
		f.Limit = 100
		got, _, err := s.History(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if n := count(EventFilter{Categories: []string{"session"}}); n != 4 {
		t.Errorf("category session = %d, want 4", n)
	}
	if n := count(EventFilter{Actor: "sceneid:bob"}); n != 3 {
		t.Errorf("actor bob = %d, want 3", n)
	}
	if n := count(EventFilter{Query: "DEMO_3"}); n != 1 {
		t.Errorf("query DEMO_3 = %d, want 1", n)
	}
	// "_" must match literally, not as a LIKE wildcard.
	if n := count(EventFilter{Query: "o_3"}); n != 1 {
		t.Errorf("query o_3 = %d, want 1 (only demo_3)", n)
	}
	if n := count(EventFilter{Query: "refused"}); n != 1 {
		t.Errorf("query in details = %d, want 1", n)
	}
	if n := count(EventFilter{From: t0.Add(2 * time.Minute), To: t0.Add(4 * time.Minute)}); n != 3 {
		t.Errorf("time window = %d, want 3 (two audit entries and the node change)", n)
	}
	if _, _, err := s.History(ctx, EventFilter{Limit: 3, Cursor: "garbage"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor: %v", err)
	}

	var exported []string
	if err := s.HistoryEach(ctx, EventFilter{Categories: []string{"repo"}}, 2, func(e Event) error {
		exported = append(exported, e.ID)
		return nil
	}); err != nil || len(exported) != 2 {
		t.Errorf("export = %v, %v", exported, err)
	}
	actors, err := s.HistoryActors(ctx)
	if err != nil || strings.Join(actors, ",") != "forgesync,sceneid:alice,sceneid:bob" {
		t.Errorf("actors = %v, %v", actors, err)
	}
}

func TestReplicaSync(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if err := s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo"}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Repositories(ctx)
	id := recs[0].ID
	if _, err := s.SetPrimary(ctx, id, "se"); err != nil {
		t.Fatal(err)
	}

	save := func(state string, at time.Time, refs map[string]string) {
		t.Helper()
		if err := s.SaveReplicaSync(ctx, ReplicaSync{RepositoryID: id, Node: "dk", State: state, LastAttemptAt: at, RefsUpdated: 2}, refs); err != nil {
			t.Fatal(err)
		}
	}
	save("synced", t0, map[string]string{"refs/heads/main": "aaa", "refs/tags/v1": "ttt"})
	save("conflict", t0.Add(time.Minute), nil) // refs untouched
	save("error", t0.Add(2*time.Minute), nil)

	st, err := s.ReplicaSyncs(ctx, id)
	if err != nil || len(st) != 1 {
		t.Fatalf("syncs = %+v, %v", st, err)
	}
	if st[0].State != "error" || !st[0].LastSuccessAt.Equal(t0) || !st[0].OutOfSyncSince.Equal(t0.Add(time.Minute)) {
		t.Errorf("streak should start at the first non-synced attempt: %+v", st[0])
	}
	refs, _ := s.ReplicatedRefs(ctx, id, "dk")
	if len(refs) != 2 || refs["refs/heads/main"] != "aaa" {
		t.Errorf("refs kept across attempts without refs: %v", refs)
	}

	save("synced", t0.Add(3*time.Minute), map[string]string{"refs/heads/main": "bbb"})
	st, _ = s.ReplicaSyncs(ctx, id)
	refs, _ = s.ReplicatedRefs(ctx, id, "dk")
	if st[0].OutOfSyncSince != nil || !st[0].LastSuccessAt.Equal(t0.Add(3*time.Minute)) || len(refs) != 1 || refs["refs/heads/main"] != "bbb" {
		t.Errorf("after success: %+v, refs %v", st[0], refs)
	}

	counts, err := s.ReplicationCounts(ctx)
	if err != nil || counts["synced"] != 1 {
		t.Errorf("counts = %v, %v", counts, err)
	}
	// If dk becomes the primary, its old replica row no longer counts.
	s.SetPrimary(ctx, id, "dk")
	if counts, _ = s.ReplicationCounts(ctx); len(counts) != 0 {
		t.Errorf("counts after primary change = %v", counts)
	}
}
