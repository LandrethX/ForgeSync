package replication

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// collabSetup is a primary and two replicas that all have the repository,
// each with its own view of who it's shared with.
func collabSetup(t *testing.T) (*Engine, *memStore, map[string]*fakeAPI) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	for _, n := range []string{"se", "dk", "de"} {
		rec.Replicas = append(rec.Replicas, store.Replica{Node: n, Present: true})
	}
	st := newMemStore(rec)
	var nodes []Node
	for _, n := range []string{"se", "dk", "de"} {
		// The people already have SceneID accounts on every node, as they
		// would once they have signed in anywhere.
		for _, login := range []string{"bob", "carol"} {
			apis[n].users[login] = forgejo.User{Login: login, SourceID: 1, LoginName: login + "-sub"}
		}
		nodes = append(nodes, Node{Name: n, URL: "http://" + n, User: testUser, Token: testToken,
			API: apis[n], SceneIDSourceID: 1})
	}
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{Collaborators: true}, slog.New(slog.DiscardHandler))
	return e, st, apis
}

func who(a *fakeAPI) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for login, perm := range a.collabs {
		out = append(out, login+":"+perm)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

func TestCollaboratorsSpreadBothWays(t *testing.T) {
	e, st, apis := collabSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	rec, _ := st.Repository(ctx, st.rec.ID)

	// Shared with someone on the primary.
	apis["se"].collabs = map[string]string{"bob": "write"}
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := who(apis[n]); got != "bob:write" {
			t.Fatalf("%s: %q", n, got)
		}
	}
	rec, _ = st.Repository(ctx, st.rec.ID)
	if rec.BaseCollaborators != "bob:write" {
		t.Fatalf("base = %q", rec.BaseCollaborators)
	}
	for _, a := range apis {
		a.calls = nil
	}
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("a settled run wrote on %s: %v", n, a.calls)
		}
	}

	// Someone else added on a replica reaches the others.
	apis["de"].collabs["carol"] = "read"
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := who(apis[n]); got != "bob:write,carol:read" {
			t.Errorf("%s: %q", n, got)
		}
	}

	// Taken away on a replica: taken away everywhere.
	rec, _ = st.Repository(ctx, st.rec.ID)
	delete(apis["dk"].collabs, "bob")
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := who(apis[n]); got != "carol:read" {
			t.Errorf("%s after the removal: %q", n, got)
		}
	}
}

// Changing what someone may do must not take their access away in between.
func TestChangingSomeonesAccessKeepsIt(t *testing.T) {
	e, st, apis := collabSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	for _, n := range []string{"se", "dk", "de"} {
		apis[n].collabs = map[string]string{"bob": "read"}
	}
	rec, _ := st.Repository(ctx, st.rec.ID)
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	rec, _ = st.Repository(ctx, st.rec.ID)
	if rec.BaseCollaborators != "bob:read" {
		t.Fatalf("base = %q", rec.BaseCollaborators)
	}

	apis["se"].collabs["bob"] = "write"
	for _, a := range apis {
		a.calls = nil
	}
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := who(apis[n]); got != "bob:write" {
			t.Errorf("%s: %q", n, got)
		}
	}
	// The grant updates; nobody is ungranted on the way.
	for n, a := range apis {
		if strings.Contains(strings.Join(a.calls, " "), "ungrant") {
			t.Errorf("%s took the access away first: %v", n, a.calls)
		}
	}
}

// A node that won't take someone leaves them half-granted, which must
// never read as the access being withdrawn everywhere.
func TestSomeoneANodeWontTake(t *testing.T) {
	e, st, apis := collabSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	apis["de"].refusesGrant = "bob"
	apis["se"].collabs = map[string]string{"bob": "write"}
	rec, _ := st.Repository(ctx, st.rec.ID)
	for i := 0; i < 3; i++ {
		e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
		rec, _ = st.Repository(ctx, st.rec.ID)
		if got := who(apis["se"]); got != "bob:write" {
			t.Fatalf("round %d: se lost bob: %q", i, got)
		}
		if got := who(apis["dk"]); got != "bob:write" {
			t.Fatalf("round %d: dk %q", i, got)
		}
		if rec.BaseCollaborators != "" {
			t.Fatalf("round %d: the base moved to %q", i, rec.BaseCollaborators)
		}
	}
	apis["de"].refusesGrant = ""
	e.syncCollaborators(ctx, rec, e.nodes["se"], healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := who(apis[n]); got != "bob:write" {
			t.Errorf("%s: %q", n, got)
		}
	}
}
