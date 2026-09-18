package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/auth"
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
