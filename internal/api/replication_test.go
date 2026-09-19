package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/leader"
	"scenegit.org/forgesync/internal/store"
)

type fakeReplicator struct{ triggered []string }

func (f *fakeReplicator) Trigger(_ context.Context, id string) (bool, error) {
	f.triggered = append(f.triggered, id)
	return len(f.triggered) == 1, nil // the second call finds it already running
}

func TestReplicateNow(t *testing.T) {
	path := "/api/v1/repositories/" + demoID + "/replicate"

	f, op := sessionAs(t, auth.Operator)
	if rec := f.do(req{method: "POST", path: path, cookie: op, csrf: true}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "turned off") {
		t.Errorf("replication off = %d %s", rec.Code, rec.Body)
	}

	f, viewer := sessionAs(t, auth.Viewer)
	f.srv.Replication = &fakeReplicator{}
	f.h = f.srv.Handler()
	if rec := f.do(req{method: "POST", path: path, cookie: viewer, csrf: true}); rec.Code != 403 {
		t.Errorf("viewer = %d, want 403", rec.Code)
	}

	f, op = sessionAs(t, auth.Operator)
	repl := &fakeReplicator{}
	f.srv.Replication = repl
	f.h = f.srv.Handler()
	if rec := f.do(req{method: "POST", path: path, cookie: op, csrf: true}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "primary") {
		t.Errorf("no primary = %d %s", rec.Code, rec.Body)
	}
	f.db.repos[0].PrimaryNode = "se"
	rec := f.do(req{method: "POST", path: path, cookie: op, csrf: true})
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"queued":true`) || len(repl.triggered) != 1 {
		t.Errorf("operator = %d %s", rec.Code, rec.Body)
	}
	rec = f.do(req{method: "POST", path: path, cookie: op, csrf: true})
	if !strings.Contains(rec.Body.String(), `"running":true`) {
		t.Errorf("already running = %s", rec.Body)
	}
	audited := 0
	for _, e := range f.db.audit {
		if e.Action == "repo.replicate_requested" {
			audited++
		}
	}
	if audited != 1 {
		t.Errorf("%d audit entries, want 1 (only the queued request)", audited)
	}
}

func TestRepositoryReplicationState(t *testing.T) {
	f := newFixture("s3cret")
	withRepos(f)
	f.db.repos[0].PrimaryNode = "se"
	now := time.Now()
	f.db.syncs = []store.ReplicaSync{
		{RepositoryID: demoID, Node: "dk", State: "conflict", LastAttemptAt: now},
		{RepositoryID: demoID, Node: "se", State: "synced", LastAttemptAt: now}, // from when se was a replica
	}

	var repo Repository
	json.Unmarshal(f.do(req{path: "/api/v1/repositories/" + demoID, bearer: "s3cret"}).Body.Bytes(), &repo)
	if repo.Replication == nil || repo.Replication.Enabled || len(repo.Replication.Replicas) != 0 {
		t.Errorf("replication off: %+v", repo.Replication)
	}

	f.srv.Replication = &fakeReplicator{}
	f.h = f.srv.Handler()
	json.Unmarshal(f.do(req{path: "/api/v1/repositories/" + demoID, bearer: "s3cret"}).Body.Bytes(), &repo)
	if !repo.Replication.Enabled || len(repo.Replication.Replicas) != 1 || repo.Replication.Replicas[0].Node != "dk" {
		t.Errorf("replication on: %+v (the primary's own row must be left out)", repo.Replication)
	}

	var o Overview
	json.Unmarshal(f.do(req{path: "/api/v1/overview", bearer: "s3cret"}).Body.Bytes(), &o)
	if !o.Replication.Enabled || o.Replication.Counts["conflict"] != 1 {
		t.Errorf("overview = %+v", o.Replication)
	}
}

// The nodes page asks what each node has sent, so a person can see where
// replication is coming from without opening every repository.
func TestReplicationSources(t *testing.T) {
	f, admin := sessionAs(t, auth.Viewer)
	f.srv.Replication = &fakeReplicator{}
	at := time.Now().Add(-time.Minute)
	f.db.pairs = []store.SourcePair{
		{From: "se", To: "dk", Repositories: 4, InSync: 3, LastSuccessAt: &at, LastAttemptAt: &at},
	}
	rec := f.do(req{path: "/api/v1/replication/sources", cookie: admin})
	if rec.Code != 200 {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Enabled bool               `json:"enabled"`
		Pairs   []store.SourcePair `json:"pairs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || len(out.Pairs) != 1 || out.Pairs[0].From != "se" || out.Pairs[0].InSync != 3 {
		t.Fatalf("answer = %+v", out)
	}

	// With replication off, the page is told so rather than shown nothing.
	f.srv.Replication = nil
	rec = f.do(req{path: "/api/v1/replication/sources", cookie: admin})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) ||
		!strings.Contains(rec.Body.String(), `"pairs":[]`) {
		t.Fatalf("with replication off = %d %s", rec.Code, rec.Body)
	}
}

// The dashboard shows a card per controller, so a person can see both
// without signing in to each: where it is, and which one is acting.
func TestOverviewListsEveryController(t *testing.T) {
	f, admin := sessionAs(t, auth.Viewer)
	f.srv.ControllerName = "forgesync-b"
	f.srv.ControllerBeat = 5 * time.Second
	f.srv.Leader = &fakeLeader{leader.State{Leading: false, Name: "forgesync-a", URL: "http://a:8090"}}
	now := time.Now()
	f.db.controllers = []store.ControllerRecord{
		{Name: "forgesync-a", URL: "http://a:8090", Address: "10.0.0.1", Version: "dev",
			StartedAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Second)},
		{Name: "forgesync-b", URL: "http://b:8091", Address: "10.0.0.2", Version: "dev",
			StartedAt: now.Add(-time.Minute), LastSeenAt: now.Add(-time.Second)},
		{Name: "forgesync-old", URL: "http://old:8090", Address: "10.0.0.9",
			StartedAt: now.Add(-48 * time.Hour), LastSeenAt: now.Add(-time.Hour)},
	}

	rec := f.do(req{path: "/api/v1/overview", cookie: admin})
	if rec.Code != 200 {
		t.Fatalf("GET overview = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Controllers []struct {
			Name, Role, Address string
			Self                bool
		} `json:"controllers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Controllers) != 3 {
		t.Fatalf("controllers = %+v", out.Controllers)
	}
	roles := map[string]string{}
	for _, c := range out.Controllers {
		roles[c.Name] = c.Role
	}
	// The one holding the lease leads; the other is standing by; one that
	// stopped checking in isn't promised to be ready.
	if roles["forgesync-a"] != "leader" || roles["forgesync-b"] != "standby" || roles["forgesync-old"] != "unknown" {
		t.Errorf("roles = %v", roles)
	}
	for _, c := range out.Controllers {
		if (c.Name == "forgesync-b") != c.Self {
			t.Errorf("%s: self = %v", c.Name, c.Self)
		}
	}
	if out.Controllers[0].Address != "10.0.0.1" {
		t.Errorf("no address: %+v", out.Controllers[0])
	}
}

// Promoting is asked of whichever controller a person is looking at,
// which is usually the standby: it writes the choice, and the leader
// steps aside of its own accord.
func TestChoosingWhichControllerLeads(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	f.srv.ControllerName = "forgesync-b"
	f.srv.Leader = &fakeLeader{leader.State{Leading: false, Name: "forgesync-a"}}
	now := time.Now()
	f.db.controllers = []store.ControllerRecord{
		{Name: "forgesync-a", Priority: 1, LastSeenAt: now},
		{Name: "forgesync-b", Priority: 2, LastSeenAt: now},
	}

	// A standby answers this one even though it turns away every other write.
	rec := f.do(req{method: "PUT", path: "/api/v1/leadership", cookie: admin, csrf: true,
		body: `{"controller":"forgesync-b"}`})
	if rec.Code != 200 {
		t.Fatalf("PUT leadership on a standby = %d %s", rec.Code, rec.Body)
	}
	if f.db.chosen.Controller != "forgesync-b" {
		t.Fatalf("chosen = %+v", f.db.chosen)
	}
	if !audited(f, "leadership.chosen") {
		t.Error("not audited")
	}
	// The overview says so, and marks the card.
	body := f.do(req{path: "/api/v1/overview", cookie: admin}).Body.String()
	if !strings.Contains(body, `"chosen":{"controller":"forgesync-b"`) {
		t.Errorf("overview = %s", body)
	}

	// A name nobody has heard of is refused rather than written down.
	rec = f.do(req{method: "PUT", path: "/api/v1/leadership", cookie: admin, csrf: true,
		body: `{"controller":"forgesync-z"}`})
	if rec.Code != 400 {
		t.Errorf("unknown controller = %d %s", rec.Code, rec.Body)
	}

	// Clearing it goes back to the configured order.
	if rec := f.do(req{method: "DELETE", path: "/api/v1/leadership", cookie: admin, csrf: true}); rec.Code != 200 {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	if f.db.chosen.Controller != "" {
		t.Errorf("still chosen: %+v", f.db.chosen)
	}

	// It's an Administrator's decision.
	g, op := sessionAs(t, auth.Operator)
	if rec := g.do(req{method: "PUT", path: "/api/v1/leadership", cookie: op, csrf: true,
		body: `{"controller":"forgesync-a"}`}); rec.Code != 403 {
		t.Errorf("as operator = %d", rec.Code)
	}
}
