package replication

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

func releaseSetup(t *testing.T) (*Engine, *memStore, map[string]*fakeAPI, store.RepositoryRecord) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	var nodes []Node
	for _, n := range []string{"se", "dk", "de"} {
		rec.Replicas = append(rec.Replicas, store.Replica{Node: n, Present: true})
		a := apis[n]
		a.name = n
		a.as = testUser
		a.releases = map[string][]*forgejo.Release{}
		a.tags = []string{"v1.0"} // replication has already put the tag everywhere
		a.users["alice"] = forgejo.User{Login: "alice", SourceID: 1, LoginName: "alice-sub"}
		nodes = append(nodes, Node{Name: n, URL: "http://" + n, User: testUser, Token: testToken,
			API: a, SceneIDSourceID: 1})
	}
	st := newMemStore(rec)
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{Releases: true}, slog.New(slog.DiscardHandler))
	return e, st, apis, rec
}

func releasesOn(a *fakeAPI) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.releases["alice/demo"] {
		files := []string{}
		for _, x := range r.Assets {
			files = append(files, x.Name)
		}
		out = append(out, r.TagName+" "+r.Title+" ["+strings.Join(files, ",")+"]")
	}
	return strings.Join(out, " | ")
}

func TestReleasesAndTheirFilesTravel(t *testing.T) {
	e, st, apis, rec := releaseSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}

	// Published on the primary, with a file.
	apis["se"].releases["alice/demo"] = []*forgejo.Release{{ID: 1, TagName: "v1.0", Title: "Version 1.0",
		Body: "The first one.", Author: forgejo.User{Login: "alice"},
		Assets: []forgejo.ReleaseAsset{{ID: 9, Name: "intro.bin", Size: 4, DownloadURL: "http://se/assets/9"}}}}
	apis["se"].assetBytes = map[int64][]byte{9: []byte("demo")}

	e.syncReleases(ctx, rec, healthy)
	want := "v1.0 Version 1.0 [intro.bin]"
	for _, n := range []string{"se", "dk", "de"} {
		if got := releasesOn(apis[n]); got != want {
			t.Fatalf("%s: %q", n, got)
		}
	}
	rec, _ = st.Repository(ctx, rec.ID)
	if !strings.HasPrefix(rec.BaseReleases, "v1.0:") || rec.BaseReleaseAssets != "v1.0|4|intro.bin" {
		t.Fatalf("what was agreed = %q %q", rec.BaseReleases, rec.BaseReleaseAssets)
	}
	for _, a := range apis {
		a.calls = nil
	}
	e.syncReleases(ctx, rec, healthy)
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("a settled run wrote on %s: %v", n, a.calls)
		}
	}

	// The words change on a replica: an edit everywhere, and the file
	// isn't thrown away and uploaded again.
	apis["de"].mu.Lock()
	apis["de"].releases["alice/demo"][0].Body = "The first one, with notes."
	apis["de"].mu.Unlock()
	for _, a := range apis {
		a.calls = nil
	}
	e.syncReleases(ctx, rec, healthy)
	for _, n := range []string{"se", "dk"} {
		if got := apis[n].releases["alice/demo"][0].Body; got != "The first one, with notes." {
			t.Errorf("%s body = %q", n, got)
		}
		if strings.Contains(strings.Join(apis[n].calls, " "), "unpublish") {
			t.Errorf("%s republished instead of editing: %v", n, apis[n].calls)
		}
		if len(apis[n].releases["alice/demo"][0].Assets) != 1 {
			t.Errorf("%s lost the file", n)
		}
	}

	// Taken down on a replica: gone everywhere.
	rec, _ = st.Repository(ctx, rec.ID)
	apis["dk"].mu.Lock()
	apis["dk"].releases["alice/demo"] = nil
	apis["dk"].mu.Unlock()
	e.syncReleases(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := releasesOn(apis[n]); got != "" {
			t.Errorf("%s still has %q", n, got)
		}
	}
}

// A release waits for its tag, which replication puts there first, and a
// draft isn't published so it doesn't travel at all.
func TestAReleaseWaitsForItsTagAndDraftsStayPut(t *testing.T) {
	e, st, apis, rec := releaseSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	apis["de"].tags = nil // the tag hasn't reached de yet

	apis["se"].releases["alice/demo"] = []*forgejo.Release{
		{ID: 1, TagName: "v1.0", Title: "Version 1.0", Author: forgejo.User{Login: "alice"}},
		{ID: 2, TagName: "v2.0", Title: "Not ready", Draft: true, Author: forgejo.User{Login: "alice"}},
	}
	e.syncReleases(ctx, rec, healthy)
	if got := releasesOn(apis["dk"]); got != "v1.0 Version 1.0 []" {
		t.Errorf("dk: %q", got)
	}
	if got := releasesOn(apis["de"]); got != "" {
		t.Errorf("de published without the tag: %q", got)
	}
	for _, n := range []string{"dk", "de"} {
		if strings.Contains(releasesOn(apis[n]), "Not ready") {
			t.Errorf("%s got the draft", n)
		}
	}
	rec, _ = st.Repository(ctx, rec.ID)
	if rec.BaseReleases != "" {
		t.Errorf("the base moved while a node was still waiting: %q", rec.BaseReleases)
	}

	// The tag arrives.
	apis["de"].tags = []string{"v1.0"}
	e.syncReleases(ctx, rec, healthy)
	if got := releasesOn(apis["de"]); got != "v1.0 Version 1.0 []" {
		t.Errorf("de after the tag arrived: %q", got)
	}
}

// Releases can be turned off for a repository on a node -- a fork has
// them off by default -- and the endpoint then answers 404. That's an
// answer, not a failure: the node is passed over, the others are brought
// together as usual, and nothing is said about it every round.
func TestANodeWithReleasesOffIsSkippedQuietly(t *testing.T) {
	e, st, apis, rec := releaseSetup(t)
	ctx := context.Background()
	apis["de"].releasesOff = true
	apis["se"].releases["alice/demo"] = []*forgejo.Release{{ID: 1, TagName: "v1.0", Title: "Version 1.0",
		Body: "The first one.", Author: forgejo.User{Login: "alice"}}}

	e.syncReleases(ctx, rec, map[string]bool{"se": true, "dk": true, "de": true})

	if got := releasesOn(apis["dk"]); got != "v1.0 Version 1.0 []" {
		t.Errorf("dk: %q", got)
	}
	if got := releasesOn(apis["de"]); got != "" {
		t.Errorf("de was written to although releases are off there: %q", got)
	}
	if len(st.found) != 0 {
		t.Errorf("a node with releases off became a conflict: %+v", st.found)
	}
}
