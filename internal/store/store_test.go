package store

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestAssignPrimaries(t *testing.T) {
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
	scan := func(node string, users []ScannedUser, repos ...ScannedRepo) {
		t.Helper()
		if err := s.RecordNodeUsers(ctx, node, t0.Add(time.Second), users); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordNodeScan(ctx, node, t0, t0.Add(time.Second), repos); err != nil {
			t.Fatal(err)
		}
	}
	// alice registered on DK (her SE account came later); bob on SE, and
	// ForgeSync created his DK account with the same creation time.
	alice := func(c time.Time) ScannedUser { return ScannedUser{Login: "alice", Sub: "sub-a", Created: c} }
	bob := func(c time.Time) ScannedUser { return ScannedUser{Login: "bob", Sub: "sub-b", Created: c} }
	if err := s.NoteCreatedAccount(ctx, "dk", "Bob"); err != nil {
		t.Fatal(err)
	}
	scan("se", []ScannedUser{alice(t0.Add(-day)), bob(t0.Add(-5 * day))},
		ScannedRepo{FullName: "alice/made-on-se", Created: t0.Add(-day)},    // follows alice: DK
		ScannedRepo{FullName: "bob/tools", Created: t0.Add(-day)},           // follows bob: SE
		ScannedRepo{FullName: "team/app", Created: t0.Add(-day)},            // an organization: origin
		ScannedRepo{FullName: "alice/chosen", Created: t0.Add(-day)},        // manual, kept
		ScannedRepo{FullName: "siteadmin/notes", Created: t0.Add(-3 * day)}, // local owner: origin SE
		ScannedRepo{FullName: "team/mirrored", Created: t0.Add(-3 * day), Mirror: true},
		ScannedRepo{FullName: "alice/mirror-only", Created: t0.Add(-day), Mirror: true}) // no primary at all
	scan("dk", []ScannedUser{alice(t0.Add(-2 * day)), bob(t0.Add(-5 * day))},
		ScannedRepo{FullName: "team/app", Created: t0.Add(-2 * day)},
		ScannedRepo{FullName: "team/mirrored", Created: t0.Add(-2 * day)},
		ScannedRepo{FullName: "alice/chosen", Created: t0.Add(-2 * day)})

	// DE has never been scanned, so it could hide an earlier account: nothing is assigned.
	if got, ok, err := s.AssignPrimaries(ctx, nodes); err != nil || ok || len(got.Homes)+len(got.Primaries) != 0 {
		t.Fatalf("with DE unscanned: %+v %v %v", got, ok, err)
	}
	scan("de", nil)

	byName := func() map[string]RepositoryRecord {
		all, _ := s.Repositories(ctx)
		m := map[string]RepositoryRecord{}
		for _, r := range all {
			m[r.FullName] = r
		}
		return m
	}
	if _, err := s.SetPrimary(ctx, byName()["alice/chosen"].ID, "se"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.AssignPrimaries(ctx, nodes)
	if err != nil || !ok {
		t.Fatalf("assign: ok %v err %v", ok, err)
	}
	homes := map[string]string{}
	for _, h := range got.Homes {
		homes[h.Login] = h.Node
	}
	if len(homes) != 2 || homes["alice"] != "dk" || homes["bob"] != "se" {
		t.Errorf("homes = %v, want alice dk (registered there), bob se (dk copy is ForgeSync's)", homes)
	}
	want := map[string][2]string{
		"alice/made-on-se": {"dk", "owner"}, "bob/tools": {"se", "owner"}, "team/app": {"dk", "origin"},
		"alice/chosen": {"se", "manual"}, "siteadmin/notes": {"se", "origin"}, "team/mirrored": {"dk", "origin"},
		"alice/mirror-only": {"", ""},
	}
	for name, r := range byName() {
		if w := want[name]; r.PrimaryNode != w[0] || r.PrimarySource != w[1] {
			t.Errorf("%s: primary %s (%s), want %s (%s)", name, r.PrimaryNode, r.PrimarySource, w[0], w[1])
		}
	}
	if len(got.Primaries) != 5 {
		t.Errorf("primary changes = %+v", got.Primaries)
	}

	// An Administrator moves alice to DE: her repositories follow, except the chosen one.
	users, _ := s.Users(ctx)
	if len(users) != 2 || users[0].Login != "alice" || len(users[0].Accounts) != 2 || users[1].Accounts[0].Node != "dk" ||
		!users[1].Accounts[0].CreatedByUs || users[1].Accounts[1].CreatedByUs {
		t.Fatalf("users = %+v", users)
	}
	if prev, err := s.SetUserHome(ctx, users[0].ID, "de"); err != nil || prev != "dk" {
		t.Fatalf("set home: %q %v", prev, err)
	}
	if _, err := s.SetUserHome(ctx, users[0].ID, ""); err == nil {
		t.Error("a user's primary site was cleared")
	}
	got, _, _ = s.AssignPrimaries(ctx, nodes)
	if len(got.Homes) != 0 || len(got.Primaries) != 1 || got.Primaries[0].FullName != "alice/made-on-se" ||
		got.Primaries[0].From != "dk" || got.Primaries[0].To != "de" {
		t.Errorf("after moving alice: %+v", got)
	}
	if again, _, _ := s.AssignPrimaries(ctx, nodes); len(again.Homes)+len(again.Primaries) != 0 {
		t.Errorf("third run changed %+v", again)
	}
	if u, _ := s.User(ctx, users[0].ID); u.HomeNode != "de" || u.HomeSource != "manual" {
		t.Errorf("alice = %+v", u)
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

// Every node's scan records the same users at about the same time, each in
// its own order; that must not deadlock.
func TestRecordNodeUsersConcurrently(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	nodes := []string{"se", "dk", "de", "uk", "us"}
	var recs []NodeRecord
	for _, n := range nodes {
		recs = append(recs, NodeRecord{Name: n, URL: "http://" + n})
	}
	if err := s.SyncNodes(ctx, recs); err != nil {
		t.Fatal(err)
	}
	var users []ScannedUser
	for i := range 200 {
		users = append(users, ScannedUser{Login: "u" + strconv.Itoa(i), Sub: "sub-" + strconv.Itoa(i)})
	}
	for round := range 5 {
		errs := make(chan error, len(nodes))
		for i, n := range nodes {
			own := append([]ScannedUser(nil), users...)
			if i%2 == 1 { // half the nodes list them the other way round
				for a, b := 0, len(own)-1; a < b; a, b = a+1, b-1 {
					own[a], own[b] = own[b], own[a]
				}
			}
			go func() { errs <- s.RecordNodeUsers(ctx, n, time.Now().Add(time.Duration(round)*time.Minute), own) }()
		}
		for range nodes {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
	all, _ := s.Users(ctx)
	if len(all) != 200 || len(all[0].Accounts) != 5 {
		t.Fatalf("%d users, first has %d accounts", len(all), len(all[0].Accounts))
	}
}

func TestHandoffs(t *testing.T) {
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
	repo := recs[0].ID
	h := Handoff{RepositoryID: repo, Ref: "refs/heads/main", SHA: "c1", PrimaryNode: "se", Nodes: []string{"dk"},
		Branch: "refs/heads/forgesync/conflict/dk/main", PRNumber: 7, PRURL: "http://se/pulls/7", OpenedAt: t0}
	id, err := s.SaveHandoff(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveHandoff(ctx, h); err == nil {
		t.Error("a second open hand-off for the same head was accepted")
	}
	got, err := s.Handoffs(ctx, repo, true)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].State != "open" || got[0].Nodes[0] != "dk" {
		t.Fatalf("handoffs = %+v, %v", got, err)
	}
	until := t0.Add(30 * 24 * time.Hour)
	h = got[0]
	h.State, h.DecidedAt, h.BackupUntil, h.Nodes = "kept_primary", &t0, &until, []string{}
	if err := s.UpdateHandoff(ctx, h); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Handoffs(ctx, repo, true); len(got) != 1 || !got[0].BackupUntil.Equal(until) {
		t.Fatalf("after deciding: %+v", got)
	}
	h.State = "expired"
	s.UpdateHandoff(ctx, h)
	if got, _ := s.Handoffs(ctx, repo, true); len(got) != 0 {
		t.Errorf("expired still active: %+v", got)
	}
	// Once decided, the same head can be handed off again.
	if _, err := s.SaveHandoff(ctx, Handoff{RepositoryID: repo, Ref: "refs/heads/main", SHA: "c1", PrimaryNode: "se",
		Nodes: []string{"dk"}, Branch: "refs/heads/forgesync/conflict/dk/main-2", PRNumber: 8, OpenedAt: t0}); err != nil {
		t.Errorf("re-handing off: %v", err)
	}
}

// A hand-off whose last replica has been reset has no nodes left. The
// column is NOT NULL, so an empty list must stay a list.
func TestAHandoffWithNoNodesLeft(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}})
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo"}})
	repos, _ := s.Repositories(ctx)

	id, err := s.SaveHandoff(ctx, Handoff{RepositoryID: repos[0].ID, Ref: "refs/heads/main", SHA: "abc",
		PrimaryNode: "se", Nodes: nil, Branch: "forgesync/conflict/dk/main", PRNumber: 1, OpenedAt: t0})
	if err != nil {
		t.Fatalf("saving with no nodes: %v", err)
	}
	if err := s.UpdateHandoff(ctx, Handoff{ID: id, SHA: "abc", State: "kept_primary", Nodes: nil}); err != nil {
		t.Fatalf("updating with no nodes: %v", err)
	}
	hs, err := s.Handoffs(ctx, repos[0].ID, false)
	if err != nil || len(hs) != 1 || len(hs[0].Nodes) != 0 {
		t.Fatalf("hand-offs = %+v, %v", hs, err)
	}
}

func TestArchives(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	if err := s.RecordNodeScan(ctx, "dk", t0, t0, []ScannedRepo{{FullName: "alice/demo"}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Repositories(ctx)
	id := recs[0].ID
	if err := s.MarkRepositoryDeleted(ctx, id, t0); err != nil {
		t.Fatal(err)
	}
	s.MarkRepositoryDeleted(ctx, id, t0.Add(time.Hour)) // the first time counts
	if r, _ := s.Repository(ctx, id); r.DeletedAt == nil || !r.DeletedAt.Equal(t0) {
		t.Fatalf("deleted_at = %v", r.DeletedAt)
	}
	a := Archive{RepositoryID: id, Node: "dk", OriginalName: "alice/demo", ArchivedName: "alice--demo--x", State: "renamed", ArchivedAt: t0}
	aid, err := s.SaveArchive(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	until := t0.Add(30 * 24 * time.Hour)
	a.ID, a.State, a.DeleteAfter = aid, "archived", &until
	if _, err := s.SaveArchive(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Archives(ctx, id)
	if err != nil || len(got) != 1 || got[0].State != "archived" || !got[0].DeleteAfter.Equal(until) {
		t.Fatalf("archives = %+v, %v", got, err)
	}
	if err := s.UndeleteRepository(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Repository(ctx, id); r.DeletedAt != nil {
		t.Error("still deleted")
	}
	if err := s.DeleteRepository(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Repository(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("forgotten repository: %v", err)
	}
	if got, _ := s.Archives(ctx, id); len(got) != 0 {
		t.Errorf("archives outlived the repository: %+v", got)
	}
}

func TestDetectRenames(t *testing.T) {
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
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	round := 0
	scan := func(node string, repos ...ScannedRepo) {
		t.Helper()
		at := t0.Add(time.Duration(round) * time.Minute)
		if err := s.RecordNodeScan(ctx, node, at, at.Add(time.Second), repos); err != nil {
			t.Fatal(err)
		}
	}
	// alice/demo is on all three nodes; se is its primary (Forgejo id 42 there).
	scan("se", ScannedRepo{FullName: "alice/demo", ForgejoID: 42})
	scan("dk", ScannedRepo{FullName: "alice/demo", ForgejoID: 7})
	scan("de", ScannedRepo{FullName: "alice/demo", ForgejoID: 9})
	all, _ := s.Repositories(ctx)
	id := all[0].ID
	if _, err := s.SetPrimary(ctx, id, "se"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReplicaSync(ctx, ReplicaSync{RepositoryID: id, Node: "dk", State: "synced", LastAttemptAt: t0},
		map[string]string{"refs/heads/main": "abc"}); err != nil {
		t.Fatal(err)
	}

	// The owner renames it on se to alice/app; dk and de still have alice/demo.
	round++
	scan("se", ScannedRepo{FullName: "alice/app", ForgejoID: 42})
	scan("dk", ScannedRepo{FullName: "alice/demo", ForgejoID: 7})
	scan("de", ScannedRepo{FullName: "alice/demo", ForgejoID: 9})
	renames, ok, err := s.DetectRenames(ctx, nodes)
	if err != nil || !ok || len(renames) != 1 || renames[0].From != "alice/demo" || renames[0].To != "alice/app" || renames[0].RepositoryID != id {
		t.Fatalf("renames = %+v, ok %v, err %v", renames, ok, err)
	}
	rec, err := s.Repository(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, r := range rec.Replicas {
		if r.Present {
			names[r.Node] = r.FullName
		}
	}
	if rec.FullName != "alice/app" || rec.PrimaryNode != "se" || rec.PrimarySource != "manual" ||
		names["se"] != "alice/app" || names["dk"] != "alice/demo" || names["de"] != "alice/demo" {
		t.Fatalf("after the rename: %+v / %v", rec, names)
	}
	if all, _ := s.Repositories(ctx); len(all) != 1 {
		t.Errorf("repositories = %d, want the one renamed", len(all))
	}
	if refs, _ := s.ReplicatedRefs(ctx, id, "dk"); refs["refs/heads/main"] != "abc" {
		t.Errorf("replication state lost: %v", refs)
	}

	// Next round: dk was renamed; de still has the old name and keeps
	// mapping to the repository through the alias.
	round++
	scan("se", ScannedRepo{FullName: "alice/app", ForgejoID: 42})
	scan("dk", ScannedRepo{FullName: "alice/app", ForgejoID: 7})
	scan("de", ScannedRepo{FullName: "alice/demo", ForgejoID: 9})
	if renames, _, _ := s.DetectRenames(ctx, nodes); len(renames) != 0 {
		t.Errorf("detected again: %+v", renames)
	}
	all, _ = s.Repositories(ctx)
	if len(all) != 1 {
		t.Fatalf("repositories = %+v", all)
	}
	for _, r := range all[0].Replicas {
		if (r.Node == "de" && r.FullName != "alice/demo") || (r.Node == "dk" && r.FullName != "alice/app") || !r.Present {
			t.Errorf("replica %+v", r)
		}
	}

	// A new alice/demo on the primary is a new repository, not the alias.
	round++
	scan("se", ScannedRepo{FullName: "alice/app", ForgejoID: 42}, ScannedRepo{FullName: "alice/demo", ForgejoID: 43})
	scan("dk", ScannedRepo{FullName: "alice/app", ForgejoID: 7})
	scan("de", ScannedRepo{FullName: "alice/app", ForgejoID: 9})
	s.DetectRenames(ctx, nodes)
	all, _ = s.Repositories(ctx)
	if len(all) != 2 || all[0].FullName != "alice/app" || all[1].FullName != "alice/demo" || all[1].ID == id {
		t.Errorf("repositories = %+v", all)
	}
	// Every copy is renamed now, so the alias is gone.
	var aliases int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM repository_aliases`).Scan(&aliases)
	if aliases != 0 {
		t.Errorf("%d aliases left", aliases)
	}
}

func TestRenamedTo(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}})
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo", ForgejoID: 42}})
	s.RecordNodeScan(ctx, "se", t0.Add(time.Minute), t0.Add(time.Minute), []ScannedRepo{{FullName: "alice/app", ForgejoID: 42}})
	all, _ := s.Repositories(ctx)
	if to, err := s.RenamedTo(ctx, all[1].ID, "se"); err != nil || all[1].FullName != "alice/demo" || to != "alice/app" {
		t.Errorf("RenamedTo(%s) = %q, %v", all[1].FullName, to, err)
	}
	if to, _ := s.RenamedTo(ctx, all[0].ID, "se"); to != "" {
		t.Errorf("RenamedTo(alice/app) = %q", to)
	}
}

func TestIssueRecords(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}})
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo"}})
	repos, _ := s.Repositories(ctx)
	repo := repos[0].ID

	id, err := s.SaveIssue(ctx, IssueRecord{RepositoryID: repo, OriginNode: "dk", Author: "bob", CreatedAt: t0,
		BaseTitle: "t", BaseBody: "b", BaseState: "open", Copies: map[string]IssueCopy{"dk": {Number: 1, ForgejoID: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	cid, err := s.SaveComment(ctx, CommentRecord{IssueID: id, OriginNode: "dk", Author: "carol", CreatedAt: t0.Add(time.Minute),
		BaseBody: "hi", Copies: map[string]int64{"dk": 20}})
	if err != nil {
		t.Fatal(err)
	}
	// Copies are replaced on save; the base follows.
	if _, err := s.SaveIssue(ctx, IssueRecord{ID: id, BaseTitle: "t2", BaseBody: "b", BaseState: "closed",
		Copies: map[string]IssueCopy{"dk": {Number: 1, ForgejoID: 10}, "se": {Number: 2, ForgejoID: 30}}}); err != nil {
		t.Fatal(err)
	}
	s.SaveComment(ctx, CommentRecord{ID: cid, BaseBody: "hi!", Copies: map[string]int64{"dk": 20, "se": 40}})
	issues, comments, err := s.Issues(ctx, repo)
	if err != nil || len(issues) != 1 || len(comments) != 1 {
		t.Fatalf("issues %+v comments %+v err %v", issues, comments, err)
	}
	is := issues[0]
	if is.BaseTitle != "t2" || is.BaseState != "closed" || is.Author != "bob" || is.Copies["se"].Number != 2 || len(is.Copies) != 2 {
		t.Errorf("issue = %+v", is)
	}
	if c := comments[0]; c.BaseBody != "hi!" || c.Copies["se"] != 40 || c.IssueID != id {
		t.Errorf("comment = %+v", c)
	}
	// One Forgejo id belongs to one copy per node.
	if _, err := s.SaveIssue(ctx, IssueRecord{RepositoryID: repo, OriginNode: "se", Author: "x", CreatedAt: t0,
		Copies: map[string]IssueCopy{"se": {Number: 9, ForgejoID: 30}}}); err == nil {
		t.Error("a second record took the same Forgejo issue")
	}
	if err := s.DeleteIssueRecord(ctx, id); err != nil {
		t.Fatal(err)
	}
	if issues, comments, _ := s.Issues(ctx, repo); len(issues)+len(comments) != 0 {
		t.Errorf("left after deleting: %+v %+v", issues, comments)
	}
}

func TestRepoItems(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}})
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo"}})
	repos, _ := s.Repositories(ctx)
	repo := repos[0].ID

	label, err := s.SaveRepoItem(ctx, RepoItem{RepositoryID: repo, Kind: "label", OriginNode: "se",
		Base: map[string]string{"name": "bug", "color": "ee0701"}, Copies: map[string]int64{"se": 7}})
	if err != nil {
		t.Fatal(err)
	}
	// Labels and milestones have separate id sequences, so the same Forgejo
	// id on the same node is fine across kinds.
	milestone, err := s.SaveRepoItem(ctx, RepoItem{RepositoryID: repo, Kind: "milestone", OriginNode: "dk",
		Base: map[string]string{"title": "v1", "state": "open"}, Copies: map[string]int64{"dk": 7}})
	if err != nil {
		t.Fatal(err)
	}
	// Copies are replaced on save; the base follows.
	if _, err := s.SaveRepoItem(ctx, RepoItem{ID: label, Kind: "label",
		Base: map[string]string{"name": "defect", "color": "ee0701"}, Copies: map[string]int64{"se": 7, "dk": 8}}); err != nil {
		t.Fatal(err)
	}
	items, err := s.RepoItems(ctx, repo)
	if err != nil || len(items) != 2 {
		t.Fatalf("items %+v err %v", items, err)
	}
	got := map[string]RepoItem{}
	for _, it := range items {
		got[it.Kind] = it
	}
	if l := got["label"]; l.ID != label || l.OriginNode != "se" || l.Base["name"] != "defect" ||
		len(l.Copies) != 2 || l.Copies["dk"] != 8 || l.DeletedAt != nil {
		t.Errorf("label = %+v", l)
	}
	if m := got["milestone"]; m.ID != milestone || m.Base["title"] != "v1" || m.Copies["dk"] != 7 {
		t.Errorf("milestone = %+v", m)
	}
	// One node's Forgejo label id belongs to one item.
	if _, err := s.SaveRepoItem(ctx, RepoItem{RepositoryID: repo, Kind: "label", OriginNode: "se",
		Base: map[string]string{"name": "other"}, Copies: map[string]int64{"se": 7}}); err == nil {
		t.Error("a second item took the same Forgejo label")
	}
	// Deleted on the primary: kept with deleted_at until the copies go.
	gone := t0.Add(time.Hour)
	if _, err := s.SaveRepoItem(ctx, RepoItem{ID: milestone, Kind: "milestone",
		Base: map[string]string{"title": "v1"}, DeletedAt: &gone, Copies: map[string]int64{"dk": 7}}); err != nil {
		t.Fatal(err)
	}
	items, _ = s.RepoItems(ctx, repo)
	for _, it := range items {
		if it.ID == milestone && (it.DeletedAt == nil || !it.DeletedAt.Equal(gone)) {
			t.Errorf("deleted_at = %v", it.DeletedAt)
		}
	}

	// An issue's labels and milestone are remembered as item ids.
	issue, err := s.SaveIssue(ctx, IssueRecord{RepositoryID: repo, OriginNode: "se", Author: "bob", CreatedAt: t0,
		BaseTitle: "t", BaseState: "open", BaseLabels: label, BaseMilestone: milestone, BaseAssignees: "alice,bob",
		BaseReactions: "alice:+1,bob:heart", BaseAttachments: "14:note.txt",
		IsPull: true, HeadBranch: "topic/x", BaseBranch: "main", BaseReviews: "0123456789abcdef",
		Copies: map[string]IssueCopy{"se": {Number: 1, ForgejoID: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	issues, _, _ := s.Issues(ctx, repo)
	if len(issues) != 1 || issues[0].BaseLabels != label || issues[0].BaseMilestone != milestone ||
		issues[0].BaseAssignees != "alice,bob" || issues[0].BaseReactions != "alice:+1,bob:heart" ||
		issues[0].BaseAttachments != "14:note.txt" || !issues[0].IsPull ||
		issues[0].HeadBranch != "topic/x" || issues[0].BaseBranch != "main" ||
		issues[0].BaseReviews != "0123456789abcdef" {
		t.Errorf("issue = %+v", issues)
	}
	// A comment keeps its own reactions.
	if _, err := s.SaveComment(ctx, CommentRecord{IssueID: issue, OriginNode: "se", Author: "bob", CreatedAt: t0,
		BaseBody: "hi", BaseReactions: "carol:rocket", BaseAttachments: "9:run.log",
		Copies: map[string]int64{"se": 99}}); err != nil {
		t.Fatal(err)
	}
	if _, comments, _ := s.Issues(ctx, repo); len(comments) != 1 || comments[0].BaseReactions != "carol:rocket" ||
		comments[0].BaseAttachments != "9:run.log" {
		t.Errorf("comment = %+v", comments)
	}
	if _, err := s.SaveIssue(ctx, IssueRecord{ID: issue, BaseTitle: "t", BaseState: "open",
		Copies: map[string]IssueCopy{"se": {Number: 1, ForgejoID: 10}}}); err != nil {
		t.Fatal(err)
	}
	if issues, _, _ := s.Issues(ctx, repo); issues[0].BaseLabels != "" || issues[0].BaseMilestone != "" ||
		issues[0].BaseAssignees != "" || issues[0].BaseReactions != "" || issues[0].BaseAttachments != "" {
		t.Errorf("labels not cleared: %+v", issues[0])
	}

	if err := s.DeleteRepoItem(ctx, label); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.RepoItems(ctx, repo); len(items) != 1 {
		t.Errorf("left after deleting: %+v", items)
	}
}

func TestLeadership(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Nobody has ever led.
	l, err := s.Leadership(ctx)
	if err != nil || l.Holder != "" || l.Held() {
		t.Fatalf("empty leadership = %+v, %v", l, err)
	}

	a, err := s.AcquireLease(ctx, "aaa", "forgesync-a", "http://a:8090", time.Minute)
	if err != nil || a.Holder != "aaa" || !a.Held() || a.For() > time.Minute {
		t.Fatalf("a = %+v, %v", a, err)
	}
	// The other controller doesn't get it while the lease is in force, and
	// learns who has it.
	b, err := s.AcquireLease(ctx, "bbb", "forgesync-b", "http://b:8091", time.Minute)
	if err != nil || b.Holder != "aaa" || b.Name != "forgesync-a" || b.URL != "http://a:8090" {
		t.Fatalf("b = %+v, %v", b, err)
	}
	// Renewing keeps the same leadership: acquired_at doesn't move.
	again, err := s.AcquireLease(ctx, "aaa", "forgesync-a", "http://a:8090", time.Minute)
	if err != nil || !again.AcquiredAt.Equal(a.AcquiredAt) || !again.RenewedAt.After(a.RenewedAt) {
		t.Fatalf("renewed = %+v (was %+v), %v", again, a, err)
	}

	// Once it has run out, the other one takes over.
	if _, err := s.AcquireLease(ctx, "aaa", "forgesync-a", "http://a:8090", -time.Second); err != nil {
		t.Fatal(err)
	}
	b, err = s.AcquireLease(ctx, "bbb", "forgesync-b", "http://b:8091", time.Minute)
	if err != nil || b.Holder != "bbb" || !b.Held() || !b.AcquiredAt.After(a.AcquiredAt) {
		t.Fatalf("takeover = %+v, %v", b, err)
	}
	// Giving it up lets the other in at once.
	if err := s.ReleaseLease(ctx, "bbb"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Leadership(ctx); l.Held() {
		t.Errorf("still held after release: %+v", l)
	}
	a, err = s.AcquireLease(ctx, "aaa", "forgesync-a", "http://a:8090", time.Minute)
	if err != nil || a.Holder != "aaa" || !a.Held() {
		t.Fatalf("after release = %+v, %v", a, err)
	}
	// Releasing is only ever one's own lease.
	if err := s.ReleaseLease(ctx, "bbb"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Leadership(ctx); !l.Held() || l.Holder != "aaa" {
		t.Errorf("someone else's release took the lease away: %+v", l)
	}
}

// SourcePairs answers "what has this node sent, and when", which the
// nodes page asks. It counts from where a repository's primary is, so a
// node's own copy never appears.
func TestSourcePairs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"},
		{Name: "dk", URL: "http://dk"}, {Name: "de", URL: "http://de"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	if err := s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/one"}, {FullName: "bob/two"}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Repositories(ctx)
	byName := map[string]string{}
	for _, r := range recs {
		byName[r.FullName] = r.ID
	}
	// alice/one is se's, bob/two is dk's.
	if _, err := s.SetPrimary(ctx, byName["alice/one"], "se"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPrimary(ctx, byName["bob/two"], "dk"); err != nil {
		t.Fatal(err)
	}
	save := func(id, node, state string, at time.Time) {
		t.Helper()
		if err := s.SaveReplicaSync(ctx, ReplicaSync{RepositoryID: id, Node: node, State: state, LastAttemptAt: at}, nil); err != nil {
			t.Fatal(err)
		}
	}
	save(byName["alice/one"], "dk", "synced", t0)
	save(byName["alice/one"], "de", "conflict", t0.Add(time.Minute))
	save(byName["alice/one"], "se", "synced", t0) // the primary's own copy: not a pair
	save(byName["bob/two"], "se", "synced", t0.Add(2*time.Minute))

	pairs, err := s.SourcePairs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SourcePair{}
	for _, p := range pairs {
		got[p.From+"->"+p.To] = p
	}
	if len(pairs) != 3 {
		t.Fatalf("pairs = %+v", pairs)
	}
	if p := got["se->dk"]; p.Repositories != 1 || p.InSync != 1 || p.LastSuccessAt == nil || !p.LastSuccessAt.Equal(t0) {
		t.Errorf("se->dk = %+v", p)
	}
	// A copy that hasn't gone through is counted, but not as in step, and
	// it has an attempt without a success.
	if p := got["se->de"]; p.Repositories != 1 || p.InSync != 0 || p.LastSuccessAt != nil ||
		p.LastAttemptAt == nil || !p.LastAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("se->de = %+v", p)
	}
	if p := got["dk->se"]; p.Repositories != 1 || p.InSync != 1 {
		t.Errorf("dk->se = %+v", p)
	}
	if _, ok := got["se->se"]; ok {
		t.Error("a node counted as sending to itself")
	}

	// A repository deleted on its primary is left out: nothing is copied
	// from it any more.
	if err := s.MarkRepositoryDeleted(ctx, byName["alice/one"], t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pairs, err = s.SourcePairs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].From != "dk" {
		t.Fatalf("after the deletion: %+v", pairs)
	}
}

// ForgeSync's own accounts: made here, checked here, and shared by both
// controllers because they share this database.
func TestAccounts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(ctx, "khav", "a-long-enough-one", "K", "administrator", "token")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Username != "khav" || a.Role != "administrator" {
		t.Fatalf("account = %+v", a)
	}

	// The same name in another case is the same account.
	if _, err := s.CreateAccount(ctx, "KHAV", "another-long-one", "", "viewer", "token"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate = %v", err)
	}

	// The right password works; the wrong one doesn't, and neither says
	// anything about the other.
	got, ok, err := s.CheckPassword(ctx, "KHAV", "a-long-enough-one")
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("sign-in = %+v %v %v", got, ok, err)
	}
	if got.LastSignIn == nil {
		t.Error("the sign-in wasn't recorded")
	}
	if _, ok, _ := s.CheckPassword(ctx, "khav", "not-the-password"); ok {
		t.Error("the wrong password was accepted")
	}
	if _, ok, _ := s.CheckPassword(ctx, "nobody", "a-long-enough-one"); ok {
		t.Error("a name that doesn't exist was accepted")
	}

	// A disabled account can't sign in, and the password can be changed.
	if _, err := s.UpdateAccount(ctx, a.ID, "K", "administrator", true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.CheckPassword(ctx, "khav", "a-long-enough-one"); ok {
		t.Error("a disabled account signed in")
	}
	if _, err := s.UpdateAccount(ctx, a.ID, "K", "operator", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountPassword(ctx, a.ID, "a-different-long-one"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.CheckPassword(ctx, "khav", "a-long-enough-one"); ok {
		t.Error("the old password still works")
	}
	if _, ok, _ := s.CheckPassword(ctx, "khav", "a-different-long-one"); !ok {
		t.Error("the new password doesn't")
	}

	// The stored hash is a hash: the password isn't in it anywhere.
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM accounts WHERE id = $1::uuid`, a.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "a-different-long-one") || !strings.HasPrefix(stored, "pbkdf2-sha256$") {
		t.Errorf("stored = %q", stored)
	}

	if n, err := s.CountAccounts(ctx); err != nil || n != 1 {
		t.Errorf("count = %d, %v", n, err)
	}
	if err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Account(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deleting: %v", err)
	}
}

// Two hashes of one password differ (they have their own salt), and each
// verifies. A hash from another password doesn't.
func TestPasswordHashing(t *testing.T) {
	one, err := HashPassword("a-long-enough-one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := HashPassword("a-long-enough-one")
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Error("two hashes of one password are the same; the salt isn't doing anything")
	}
	for _, h := range []string{one, two} {
		if ok, err := VerifyPassword(h, "a-long-enough-one"); err != nil || !ok {
			t.Errorf("verify = %v, %v", ok, err)
		}
		if ok, _ := VerifyPassword(h, "a-long-enough-two"); ok {
			t.Error("another password verified")
		}
	}
	if _, err := HashPassword(""); err == nil {
		t.Error("an empty password was hashed")
	}
	if _, err := VerifyPassword("not-a-hash", "x"); err == nil {
		t.Error("a hash that isn't one was accepted")
	}
}

// Sessions are in the database so every controller sees them, and they
// end two ways: after their lifetime, and after long enough without a
// request.
func TestSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const idle = 30 * time.Minute
	identity := []byte(`{"username":"khav","role":"administrator"}`)
	if err := s.CreateSession(ctx, "hash-1", identity, time.Now().Add(8*time.Hour), idle); err != nil {
		t.Fatal(err)
	}
	got, expires, ok, err := s.Session(ctx, "hash-1", idle, true)
	if err != nil || !ok {
		t.Fatalf("session = %s %v %v", got, ok, err)
	}
	// jsonb is stored as a document, not as the bytes given, so read it
	// back the way the caller does.
	var who struct{ Username, Role string }
	if err := json.Unmarshal(got, &who); err != nil || who.Username != "khav" || who.Role != "administrator" {
		t.Fatalf("identity = %s (%v)", got, err)
	}
	if time.Until(expires) < 7*time.Hour {
		t.Errorf("expires = %s", expires)
	}

	// Reading without touching doesn't count as activity: an open event
	// stream mustn't keep an idle session alive.
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() - interval '29 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, "hash-1", idle, false); !ok {
		t.Fatal("a session 29 minutes idle was refused")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() - interval '31 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, "hash-1", idle, true); ok {
		t.Fatal("a session 31 minutes idle was accepted")
	}

	// And the absolute lifetime ends it however busy it has been.
	if err := s.CreateSession(ctx, "hash-2", identity, time.Now().Add(-time.Minute), idle); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, "hash-2", idle, true); ok {
		t.Fatal("a session past its lifetime was accepted")
	}

	// Signing out removes it for every controller at once, and the ones
	// that have run out are cleared away.
	if err := s.CreateSession(ctx, "hash-3", identity, time.Now().Add(time.Hour), idle); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "hash-3"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, "hash-3", idle, true); ok {
		t.Fatal("a deleted session was accepted")
	}
	if _, err := s.PurgeSessions(ctx, idle); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d sessions left after the purge", left)
	}
}

// A dismissed conflict stays out of the way while it's the same
// conflict, comes back when what it says changes, and clears like any
// other once the nodes agree.
func TestDismissedConflicts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	if err := s.RecordNodeScan(ctx, "se", t0, t0, []ScannedRepo{{FullName: "alice/demo"}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.Repositories(ctx)
	id := recs[0].ID
	secret := func(missing []string) []FoundConflict {
		return []FoundConflict{{RepositoryID: id, Kind: "actions_secret_missing", Ref: "TOKEN",
			Details: map[string]any{"secret": "TOKEN", "missing": missing}}}
	}
	if _, err := s.SyncConflicts(ctx, secret([]string{"dk"}), []string{id}, nil, t0); err != nil {
		t.Fatal(err)
	}
	list, _, _, err := s.Conflicts(ctx, ConflictFilter{State: "open", Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("conflicts = %+v, %v", list, err)
	}
	cid := list[0].ID

	if err := s.DismissConflict(ctx, cid, "account:khav", "set by hand on each node", t0); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OpenConflicts(ctx); n != 0 {
		t.Fatalf("open conflicts after dismissing = %d", n)
	}

	// Found again, unchanged: it stays dismissed rather than coming back
	// every round.
	if _, err := s.SyncConflicts(ctx, secret([]string{"dk"}), []string{id}, nil, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OpenConflicts(ctx); n != 0 {
		t.Fatalf("a dismissed conflict came back unchanged: %d open", n)
	}
	one, err := s.ConflictByID(ctx, cid)
	if err != nil || one.State != "dismissed" || one.DismissedBy != "account:khav" {
		t.Fatalf("conflict = %+v, %v", one, err)
	}

	// What it says changes -- another node is missing the secret now --
	// so it's a different situation and comes back.
	if _, err := s.SyncConflicts(ctx, secret([]string{"dk", "de"}), []string{id}, nil, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OpenConflicts(ctx); n != 1 {
		t.Fatalf("open conflicts after the details changed = %d", n)
	}
	one, _ = s.ConflictByID(ctx, cid)
	if one.State != "open" || one.DismissedBy != "" {
		t.Errorf("conflict = %+v", one)
	}

	// Dismissed and then gone: it clears like any other.
	if err := s.DismissConflict(ctx, cid, "account:khav", "", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncConflicts(ctx, nil, []string{id}, nil, t0.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	one, _ = s.ConflictByID(ctx, cid)
	if one.State != "cleared" {
		t.Errorf("after the nodes agreed: %+v", one)
	}

	// Bringing one back is for when it turns out to matter.
	if _, err := s.SyncConflicts(ctx, secret([]string{"dk"}), []string{id}, nil, t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	list, _, _, _ = s.Conflicts(ctx, ConflictFilter{State: "open", Limit: 10})
	if len(list) != 1 {
		t.Fatalf("expected it open again: %+v", list)
	}
	if err := s.DismissConflict(ctx, list[0].ID, "account:khav", "", t0.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReopenConflict(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OpenConflicts(ctx); n != 1 {
		t.Errorf("after bringing it back: %d open", n)
	}
}

// Ping is what /readyz, /metrics and the overview ask, so it has to mean
// "this database takes writes", not merely "something answered". Against
// an ordinary database it passes; against a standby it says so, which is
// the case check-db-failover.sh makes for real. Set
// FORGESYNC_TEST_STANDBY_DATABASE_URL to a streaming standby to run that
// half here as well.
func TestPingWantsTheServerThatTakesWrites(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping against the test database: %v", err)
	}
	if errors.Is(s.Ping(ctx), ErrStandby) {
		t.Fatal("the test database was taken for a standby")
	}

	url := os.Getenv("FORGESYNC_TEST_STANDBY_DATABASE_URL")
	if url == "" {
		t.Skip("FORGESYNC_TEST_STANDBY_DATABASE_URL not set: the standby half isn't run here")
	}
	standby, err := Open(ctx, url)
	if err == nil {
		standby.Close()
		t.Fatal("Open accepted a standby")
	}
	if !errors.Is(err, ErrStandby) {
		t.Fatalf("Open against a standby: %v, want ErrStandby", err)
	}
}

// Nodes carry their credentials now, so a controller can be told about one
// without a file being edited. The token is sealed before it gets here;
// the store's job is only to keep the bytes and hand them back.
func TestNodesCarryTheirCredentials(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sealed := []byte{0x01, 0x02, 0x03, 0xff, 0x00, 0x7f}
	want := NodeRecord{Name: "se", URL: "http://se", Site: "SE", ServiceUser: "forgesync",
		SceneIDSourceID: 3, SealedToken: sealed, Source: "api", AddedBy: "account:khav"}
	if err := s.SaveNode(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1", len(got))
	}
	if got[0].Name != want.Name || got[0].URL != want.URL || got[0].Site != want.Site ||
		got[0].ServiceUser != want.ServiceUser || got[0].SceneIDSourceID != want.SceneIDSourceID ||
		got[0].Source != want.Source || got[0].AddedBy != want.AddedBy {
		t.Errorf("node came back as %+v, want %+v", got[0], want)
	}
	if !bytes.Equal(got[0].SealedToken, sealed) {
		t.Errorf("sealed token came back as %v, want %v", got[0].SealedToken, sealed)
	}

	// Changing the address must not mean re-entering the token.
	moved := want
	moved.URL = "http://se-new"
	moved.SealedToken = nil
	if err := s.SaveNode(ctx, moved); err != nil {
		t.Fatal(err)
	}
	got, err = s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].URL != "http://se-new" {
		t.Errorf("url is %q after the move", got[0].URL)
	}
	if !bytes.Equal(got[0].SealedToken, sealed) {
		t.Error("saving without a token cleared the one that was there")
	}

	// A node registered the old way has no token of its own, which is how
	// a controller knows to keep reading the config file for it.
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "dk", URL: "http://dk", Site: "DK"}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range got {
		if n.Name == "dk" && n.SealedToken != nil {
			t.Error("a node from the config file came back with a sealed token")
		}
	}
}

func TestRetiringANodeKeepsItsHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes(ctx, []NodeRecord{{Name: "se", URL: "http://se"}, {Name: "dk", URL: "http://dk"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.RecordNodeStatus(ctx, health.Status{Node: "dk", State: health.Healthy, LastChecked: now}, health.Unknown); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireNode(ctx, "dk"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "se" {
		t.Fatalf("after forgetting dk the nodes are %+v", got)
	}
	states, err := s.NodeStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states["dk"]; ok {
		t.Error("dk's health row outlived the node")
	}

	// The history is not the node list: what dk did is still on record.
	events, _, err := s.History(ctx, EventFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var sawDK bool
	for _, e := range events {
		if e.Category == "node" && e.Target == "dk" {
			sawDK = true
		}
	}
	if !sawDK {
		t.Error("retiring dk erased its health history")
	}

	// Adding it again brings it back rather than making a second one.
	if err := s.SaveNode(ctx, NodeRecord{Name: "dk", URL: "http://dk", Site: "DK"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after adding dk again there are %d nodes, want 2", len(got))
	}
	if err := s.RetireNode(ctx, "nothing-like-this"); err == nil {
		t.Error("retiring a node that isn't there was accepted")
	}
}
