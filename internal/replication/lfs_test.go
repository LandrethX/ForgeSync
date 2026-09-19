package replication

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// lfsSetup is a primary and two replicas that all have alice/demo, with a
// commit on the primary whose file is an LFS pointer. Only the primary's
// LFS store has the object, which is what a push with git-lfs leaves.
func lfsSetup(t *testing.T) (*Engine, *memStore, map[string]*gitNode, string) {
	t.Helper()
	nodes := map[string]*gitNode{"se": newGitNode(t), "dk": newGitNode(t), "de": newGitNode(t)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	var ns []Node
	for _, name := range []string{"se", "dk", "de"} {
		nodes[name].create("alice/demo")
		rec.Replicas = append(rec.Replicas, store.Replica{Node: name, Present: true})
		ns = append(ns, Node{Name: name, URL: nodes[name].srv.URL, User: testUser, Token: testToken,
			API: newFakeAPI(nodes[name])})
	}
	st := newMemStore(rec)
	e := NewEngine(ns, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{LFS: true}, slog.New(slog.DiscardHandler))

	const content = "a big file's worth of bytes"
	w := newWorkTree(t)
	os.WriteFile(filepath.Join(w.dir, "big.bin"), []byte(pointerFor(content)), 0o644)
	runGit(t, w.dir, "add", "big.bin")
	runGit(t, w.dir, "commit", "--quiet", "-m", "add a large file")
	w.push(nodes["se"], "alice/demo", "HEAD:refs/heads/main")
	nodes["se"].putLFS(content)
	return e, st, nodes, content
}

// cache fetches the primary into the engine's cache, as a replication run
// does before the LFS pass looks for pointer files.
func (e *Engine) cacheOf(t *testing.T, rec store.RepositoryRecord) string {
	t.Helper()
	dir, err := e.git.Cache(bg(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.git.Fetch(bg(), dir, "primary", e.remote(e.nodes[rec.PrimaryNode], rec.FullName)); err != nil {
		t.Fatal(err)
	}
	return dir
}

var lfsHealthy = map[string]bool{"se": true, "dk": true, "de": true}

func TestLFSObjectsReachEveryNode(t *testing.T) {
	e, st, nodes, content := lfsSetup(t)
	rec, _ := st.Repository(bg(), st.rec.ID)
	dir := e.cacheOf(t, rec)

	e.syncLFS(bg(), dir, rec, lfsHealthy)

	for _, n := range []string{"se", "dk", "de"} {
		if !nodes[n].hasLFS(content) {
			t.Errorf("%s hasn't got the object", n)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
	// A settled run copies nothing: the batch API says they all have it.
	before := len(st.audit)
	e.syncLFS(bg(), dir, rec, lfsHealthy)
	if len(st.audit) != before {
		t.Errorf("a settled run copied again: %v", st.audit[before:])
	}
}

// An object only a replica has goes to the others too: an oid is the hash
// of its content, so there's no question of which copy is right.
func TestAnObjectOnAReplicaReachesTheRest(t *testing.T) {
	e, st, nodes, _ := lfsSetup(t)
	rec, _ := st.Repository(bg(), st.rec.ID)
	dir := e.cacheOf(t, rec)
	e.syncLFS(bg(), dir, rec, lfsHealthy)

	// A second pointer, committed on the primary, whose object was pushed
	// to a replica instead (the primary's copy has yet to arrive).
	const other = "another large file"
	w := newWorkTree(t)
	runGit(t, w.dir, "fetch", "--quiet", filepath.Join(nodes["se"].root, "alice/demo.git"), "main")
	runGit(t, w.dir, "reset", "--quiet", "--hard", "FETCH_HEAD")
	os.WriteFile(filepath.Join(w.dir, "other.bin"), []byte(pointerFor(other)), 0o644)
	runGit(t, w.dir, "add", "other.bin")
	runGit(t, w.dir, "commit", "--quiet", "-m", "another large file")
	w.push(nodes["se"], "alice/demo", "HEAD:refs/heads/main")
	nodes["de"].putLFS(other)

	rec, _ = st.Repository(bg(), st.rec.ID)
	dir = e.cacheOf(t, rec)
	e.syncLFS(bg(), dir, rec, lfsHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if !nodes[n].hasLFS(other) {
			t.Errorf("%s hasn't got the object", n)
		}
	}
}

// A node that can't take the objects -- LFS turned off there -- is said so
// plainly, and the nodes that can still get them.
func TestANodeWithoutLFSIsAConflict(t *testing.T) {
	e, st, nodes, content := lfsSetup(t)
	nodes["dk"].lfsOff = true
	rec, _ := st.Repository(bg(), st.rec.ID)
	dir := e.cacheOf(t, rec)

	e.syncLFS(bg(), dir, rec, lfsHealthy)

	if !nodes["de"].hasLFS(content) {
		t.Error("de hasn't got the object, though only dk refused")
	}
	if len(st.found) != 1 || st.found[0].Kind != LFSConflictKind || st.found[0].Ref != "dk" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if e, _ := st.found[0].Details["error"].(string); !strings.Contains(e, "404") {
		t.Errorf("the reason doesn't say what the node said: %q", e)
	}

	// Turned on again, the objects arrive and the conflict goes.
	nodes["dk"].lfsOff = false
	e.syncLFS(bg(), dir, rec, lfsHealthy)
	if !nodes["dk"].hasLFS(content) {
		t.Error("dk still hasn't got the object")
	}
	if len(st.found) != 0 {
		t.Fatalf("still conflicting: %+v", st.found)
	}
}

// An object nobody has is the repository's own problem -- someone pushed a
// pointer without the file -- not a difference between nodes.
func TestAPointerNobodyHasIsNotAConflict(t *testing.T) {
	e, st, nodes, _ := lfsSetup(t)
	rec, _ := st.Repository(bg(), st.rec.ID)
	w := newWorkTree(t)
	runGit(t, w.dir, "fetch", "--quiet", filepath.Join(nodes["se"].root, "alice/demo.git"), "main")
	runGit(t, w.dir, "reset", "--quiet", "--hard", "FETCH_HEAD")
	os.WriteFile(filepath.Join(w.dir, "lost.bin"), []byte(pointerFor("never uploaded")), 0o644)
	runGit(t, w.dir, "add", "lost.bin")
	runGit(t, w.dir, "commit", "--quiet", "-m", "a pointer without its file")
	w.push(nodes["se"], "alice/demo", "HEAD:refs/heads/main")

	dir := e.cacheOf(t, rec)
	e.syncLFS(bg(), dir, rec, lfsHealthy)
	if len(st.found) != 0 {
		t.Fatalf("conflicts = %+v", st.found)
	}
}

// The objects are found by reading, not by guessing: a small file that
// isn't a pointer is left alone, and a pointer in an old commit still
// counts.
func TestPointersAreReadFromTheRepository(t *testing.T) {
	e, st, nodes, content := lfsSetup(t)
	rec, _ := st.Repository(bg(), st.rec.ID)
	w := newWorkTree(t)
	runGit(t, w.dir, "fetch", "--quiet", filepath.Join(nodes["se"].root, "alice/demo.git"), "main")
	runGit(t, w.dir, "reset", "--quiet", "--hard", "FETCH_HEAD")
	os.WriteFile(filepath.Join(w.dir, "notes.txt"), []byte("version https://example.invalid/not-lfs\n"), 0o644)
	os.Remove(filepath.Join(w.dir, "big.bin")) // the pointer is now only in history
	runGit(t, w.dir, "add", "-A")
	runGit(t, w.dir, "commit", "--quiet", "-m", "remove the large file")
	w.push(nodes["se"], "alice/demo", "HEAD:refs/heads/main")

	dir := e.cacheOf(t, rec)
	ptrs, err := e.git.Pointers(bg(), dir, []string{"refs/forgesync/primary/heads/*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ptrs) != 1 || ptrs[0].Size != int64(len(content)) {
		t.Fatalf("pointers = %+v", ptrs)
	}
	e.syncLFS(bg(), dir, rec, lfsHealthy)
	if !nodes["dk"].hasLFS(content) {
		t.Error("the object in the older commit didn't travel")
	}
}
