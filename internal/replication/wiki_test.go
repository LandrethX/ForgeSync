package replication

import (
	"log/slog"
	"path/filepath"
	"testing"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// wikiSetup is a primary and a replica that both have alice/demo. Only the
// primary has a wiki, which is how it usually starts.
func wikiSetup(t *testing.T) (*Engine, *memStore, *gitNode, *gitNode, *workTree) {
	t.Helper()
	se, dk := newGitNode(t), newGitNode(t)
	se.create("alice/demo")
	dk.create("alice/demo")
	se.createWithCommit("alice/demo.wiki", "the primary's first page")
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se",
		Replicas: []store.Replica{{Node: "se", Present: true}, {Node: "dk", Present: true}}}
	st := newMemStore(rec)
	seAPI, dkAPI := newFakeAPI(se), newFakeAPI(dk)
	e := NewEngine([]Node{
		{Name: "se", URL: se.srv.URL, User: testUser, Token: testToken, API: seAPI},
		{Name: "dk", URL: dk.srv.URL, User: testUser, Token: testToken, API: dkAPI},
	}, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy},
		Options{Wiki: true}, slog.New(slog.DiscardHandler))
	return e, st, se, dk, newWorkTree(t)
}

// A node with no wiki gets one: Forgejo makes the repository when the
// first page is written, and the primary's history replaces that page.
func TestAWikiReachesANodeThatHasNone(t *testing.T) {
	e, st, se, dk, _ := wikiSetup(t)
	rec, _ := st.Repository(bg(), st.rec.ID)

	e.syncWiki(bg(), rec, e.nodes["se"], map[string]bool{"se": true, "dk": true})

	want := se.refs("alice/demo.wiki")
	got := dk.refs("alice/demo.wiki")
	if len(want) == 0 || got["refs/heads/main"] != want["refs/heads/main"] {
		t.Fatalf("dk's wiki = %v, want %v", got, want)
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
	// What ForgeSync wrote is remembered, so the next run is a no-op.
	if st.wikiRefs["dk"]["refs/heads/main"] != want["refs/heads/main"] {
		t.Errorf("what was written = %v", st.wikiRefs["dk"])
	}
	e.syncWiki(bg(), rec, e.nodes["se"], map[string]bool{"se": true, "dk": true})
	if got := dk.refs("alice/demo.wiki"); got["refs/heads/main"] != want["refs/heads/main"] {
		t.Errorf("a settled run moved it: %v", got)
	}
}

// Once both have it, the wiki fast-forwards like anything else, and a
// replica that has been written to on its own is a conflict rather than
// something to overwrite.
func TestAWikiFastForwardsAndDivergenceIsAConflict(t *testing.T) {
	e, st, se, dk, w := wikiSetup(t)
	rec, _ := st.Repository(bg(), st.rec.ID)
	healthy := map[string]bool{"se": true, "dk": true}
	e.syncWiki(bg(), rec, e.nodes["se"], healthy)

	// A page added on the primary reaches the replica.
	runGit(t, w.dir, "fetch", "--quiet", filepath.Join(se.root, "alice/demo.wiki.git"), "main")
	runGit(t, w.dir, "reset", "--quiet", "--hard", "FETCH_HEAD")
	ahead := w.commit("a second page")
	w.push(se, "alice/demo.wiki", "HEAD:refs/heads/main")

	e.syncWiki(bg(), rec, e.nodes["se"], healthy)
	if got := dk.refs("alice/demo.wiki")["refs/heads/main"]; got != ahead {
		t.Fatalf("dk's wiki = %s, want %s", got, ahead)
	}

	// Now each side gets a page of its own from the same starting point:
	// that's a real divergence, and ForgeSync says so rather than
	// overwriting what someone wrote on the replica.
	shared := ahead
	own := w.commit("written on the replica")
	w.push(dk, "alice/demo.wiki", "HEAD:refs/heads/main")
	runGit(t, w.dir, "reset", "--quiet", "--hard", shared)
	w.commit("and a different one on the primary")
	w.push(se, "alice/demo.wiki", "HEAD:refs/heads/main")

	st.found = nil
	e.syncWiki(bg(), rec, e.nodes["se"], healthy)
	if got := dk.refs("alice/demo.wiki")["refs/heads/main"]; got != own {
		t.Errorf("the replica's page was overwritten: %s", got)
	}
	if len(st.found) != 1 || st.found[0].Ref != "wiki:refs/heads/main" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if st.found[0].Details["wiki"] != true {
		t.Errorf("the conflict doesn't say it's the wiki: %+v", st.found[0].Details)
	}
}
