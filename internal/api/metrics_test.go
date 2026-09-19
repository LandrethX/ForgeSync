package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/leader"
	"scenegit.org/forgesync/internal/store"
)

// series returns the value of one series from the exposition, or "" if
// it isn't there.
func series(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, " "); ok && key == name {
			return value
		}
	}
	return ""
}

func TestMetricsSayWhetherForgeSyncIsDoingItsJob(t *testing.T) {
	f := newFixture("admin-token")
	seen := time.Now().Add(-30 * time.Second)
	scanned := time.Now().Add(-2 * time.Minute)
	f.health.status = []health.Status{
		{Node: "se", State: health.Healthy, LastSeen: &seen},
		{Node: "dk", State: health.Unreachable, ConsecutiveFailures: 4},
	}
	f.db.scans = []store.NodeScan{{Node: "se", Repositories: 7, LastSuccessAt: &scanned}}
	f.db.repos = []store.RepositoryRecord{
		{ID: "a", FullName: "alice/one", PrimaryNode: "se"},
		{ID: "b", FullName: "alice/two"},
	}
	f.db.conflicts = []store.Conflict{{ID: 1, State: "open"}, {ID: 2, State: "cleared"}}
	f.db.syncs = []store.ReplicaSync{{State: "synced"}, {State: "synced"}, {State: "conflict"}}
	f.srv.Replication = &fakeReplicator{}

	rec := f.do(req{path: "/metrics", bearer: "admin-token"})
	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	body := rec.Body.String()

	for name, want := range map[string]string{
		`forgesync_node_healthy{node="se"}`:                   "1",
		`forgesync_node_healthy{node="dk"}`:                   "0",
		`forgesync_node_consecutive_failures{node="dk"}`:      "4",
		`forgesync_node_state{node="dk",state="UNREACHABLE"}`: "1",
		`forgesync_node_repositories{node="se"}`:              "7",
		"forgesync_conflicts_open":                            "1",
		"forgesync_repositories":                              "2",
		"forgesync_repositories_with_primary":                 "1",
		`forgesync_replicas{state="synced"}`:                  "2",
		`forgesync_replicas{state="conflict"}`:                "1",
		"forgesync_database_up":                               "1",
	} {
		if got := series(body, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// A timestamp, not an age: whatever reads this can do the arithmetic.
	if got := series(body, `forgesync_node_last_seen_timestamp_seconds{node="se"}`); got == "" {
		t.Error("no last-seen timestamp for se")
	}
	if !strings.Contains(body, "# TYPE forgesync_node_healthy gauge") {
		t.Error("no TYPE line for forgesync_node_healthy")
	}
}

// Which controller is acting is the first thing to alert on: a standby
// looks healthy while doing nothing at all.
func TestMetricsSayWhichControllerIsActing(t *testing.T) {
	f := newFixture("admin-token")
	f.srv.Leader = &fakeLeader{leader.State{Leading: false, Name: "forgesync-a", URL: "http://a:8090"}}
	body := f.do(req{path: "/metrics", bearer: "admin-token"}).Body.String()
	if got := series(body, `forgesync_leader{leader="forgesync-a",role="standby"}`); got != "0" {
		t.Errorf("on a standby: %q\n%s", got, body)
	}

	f.srv.Leader = &fakeLeader{leader.State{Leading: true, Name: "forgesync-b"}}
	body = f.do(req{path: "/metrics", bearer: "admin-token"}).Body.String()
	if got := series(body, `forgesync_leader{leader="forgesync-b",role="leader"}`); got != "1" {
		t.Errorf("on the leader: %q", got)
	}

	// A single controller is always the one acting.
	f.srv.Leader = nil
	body = f.do(req{path: "/metrics", bearer: "admin-token"}).Body.String()
	if got := series(body, `forgesync_leader{leader="",role="single"}`); got != "1" {
		t.Errorf("single: %q", got)
	}
}

func TestMetricsNeedARole(t *testing.T) {
	f := newFixture("admin-token")
	if rec := f.do(req{path: "/metrics"}); rec.Code != 401 {
		t.Errorf("without a token = %d", rec.Code)
	}
	if rec := f.do(req{path: "/metrics", bearer: "wrong"}); rec.Code != 401 {
		t.Errorf("with the wrong token = %d", rec.Code)
	}
}

// The database being unreachable is exactly when metrics matter, so the
// endpoint answers anyway and says so.
func TestMetricsAnswerWhenTheDatabaseIsGone(t *testing.T) {
	f := newFixture("admin-token")
	f.db.pingErr = errors.New("the database is gone")
	rec := f.do(req{path: "/metrics", bearer: "admin-token"})
	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	if got := series(rec.Body.String(), "forgesync_database_up"); got != "0" {
		t.Errorf("forgesync_database_up = %q", got)
	}
}
