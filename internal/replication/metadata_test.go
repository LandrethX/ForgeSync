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

func metaSetup(t *testing.T) (*Engine, *memStore, map[string]*fakeAPI, store.RepositoryRecord) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	var nodes []Node
	for _, n := range []string{"se", "dk", "de"} {
		rec.Replicas = append(rec.Replicas, store.Replica{Node: n, Present: true})
		apis[n].repos["alice/demo"] = forgejo.Repository{FullName: "alice/demo", Description: "a demo",
			HasIssues: true, HasWiki: true, AllowMergeCommits: true, DefaultMergeStyle: "merge"}
		apis[n].topics = map[string][]string{"alice/demo": {"scene"}}
		nodes = append(nodes, Node{Name: n, URL: "http://" + n, User: testUser, Token: testToken, API: apis[n]})
	}
	st := newMemStore(rec)
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{Metadata: true}, slog.New(slog.DiscardHandler))
	return e, st, apis, rec
}

func TestRepositorySettingsAndTopicsTravel(t *testing.T) {
	e, st, apis, rec := metaSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}

	e.syncMetadata(ctx, rec, healthy)
	rec, _ = st.Repository(ctx, rec.ID)
	if rec.BaseMetadata["description"] != "a demo" || rec.BaseTopics != "scene" {
		t.Fatalf("what was agreed = %+v %q", rec.BaseMetadata, rec.BaseTopics)
	}
	for _, a := range apis {
		a.calls = nil
	}
	e.syncMetadata(ctx, rec, healthy)
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("a settled run wrote on %s: %v", n, a.calls)
		}
	}

	// Changed on a replica: everywhere.
	apis["de"].mu.Lock()
	r := apis["de"].repos["alice/demo"]
	r.Description, r.HasWiki = "a better demo", false
	apis["de"].repos["alice/demo"] = r
	apis["de"].topics["alice/demo"] = []string{"scene", "forgejo"}
	apis["de"].mu.Unlock()

	e.syncMetadata(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		r := apis[n].repos["alice/demo"]
		if r.Description != "a better demo" || r.HasWiki {
			t.Errorf("%s: description %q has_wiki %v", n, r.Description, r.HasWiki)
		}
		// The node that already had them keeps its own order; what
		// matters is that every node has the same set.
		got := append([]string(nil), apis[n].topics["alice/demo"]...)
		slices.Sort(got)
		if strings.Join(got, ",") != "forgejo,scene" {
			t.Errorf("%s topics: %v", n, apis[n].topics["alice/demo"])
		}
	}

	// Two different descriptions: a conflict, and neither is overwritten.
	rec, _ = st.Repository(ctx, rec.ID)
	for _, n := range []string{"se", "de"} {
		apis[n].mu.Lock()
		r := apis[n].repos["alice/demo"]
		r.Description = "from " + n
		apis[n].repos["alice/demo"] = r
		apis[n].mu.Unlock()
	}
	e.syncMetadata(ctx, rec, healthy)
	if len(st.found) != 1 || st.found[0].Kind != MetadataConflictKind || st.found[0].Ref != "description" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if apis["se"].repos["alice/demo"].Description != "from se" || apis["de"].repos["alice/demo"].Description != "from de" {
		t.Error("a description was overwritten")
	}
}

// Private travels one way: a repository private anywhere becomes private
// everywhere, and ForgeSync never makes one public.
func TestPrivateTravelsOneWay(t *testing.T) {
	e, st, apis, rec := metaSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	e.syncMetadata(ctx, rec, healthy)
	rec, _ = st.Repository(ctx, rec.ID)

	apis["de"].mu.Lock()
	r := apis["de"].repos["alice/demo"]
	r.Private = true
	apis["de"].repos["alice/demo"] = r
	apis["de"].mu.Unlock()

	e.syncMetadata(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if !apis[n].repos["alice/demo"].Private {
			t.Errorf("%s is still public", n)
		}
	}

	// Made public again on one node: the others stay private, and that
	// node is made private again rather than the rest being exposed.
	rec, _ = st.Repository(ctx, rec.ID)
	apis["dk"].mu.Lock()
	r = apis["dk"].repos["alice/demo"]
	r.Private = false
	apis["dk"].repos["alice/demo"] = r
	apis["dk"].mu.Unlock()

	e.syncMetadata(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if !apis[n].repos["alice/demo"].Private {
			t.Errorf("%s was made public by one node's say-so", n)
		}
	}
}
