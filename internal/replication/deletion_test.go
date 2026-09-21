package replication

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// deleteOnPrimary deletes alice/demo on se, as its owner would, and makes
// the inventory agree (se had it, doesn't now).
func deleteOnPrimary(t *testing.T, st *memStore, se *gitNode, seAPI *fakeAPI) {
	t.Helper()
	os.RemoveAll(filepath.Join(se.root, "alice/demo.git"))
	delete(seAPI.repos, "alice/demo")
	seen := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	st.rec.Replicas = []store.Replica{{Node: "se", Present: false, LastSeenAt: &seen}, {Node: "dk", Present: true}}
}

func TestDeletedOnPrimary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC)

	t.Run("copies are archived, then deleted after the backup period", func(t *testing.T) {
		e, st, se, dk, seAPI, dkAPI, w, a := setupResolve(t)
		e.now = func() time.Time { return now }
		deleteOnPrimary(t, st, se, seAPI)
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		want := "alice--demo--20260919-103000"
		if got := strings.Join(dkAPI.calls, ","); got != "rename alice/demo "+want+","+
			"create org forgesync-archive private for forgesync,transfer alice/"+want+" forgesync-archive,archive forgesync-archive/"+want {
			t.Fatalf("dk calls = %s", got)
		}
		// Everything is kept, commits included.
		if dk.refs("forgesync-archive/" + want)["refs/heads/main"] != a {
			t.Errorf("archived copy lost its commits: %v", dk.refs("forgesync-archive/"+want))
		}
		if s := st.sync("dk"); s.State != StateArchived || !strings.Contains(s.Detail, "until 2026-10-19") {
			t.Errorf("dk state = %+v", s)
		}
		if st.rec.DeletedAt == nil || len(st.checked) != 1 || st.found != nil {
			t.Errorf("deleted %v, checked %v, found %v", st.rec.DeletedAt, st.checked, st.found)
		}
		if got := actions(st, "repo."); strings.Join(got, ",") != "repo.deleted_on_primary,repo.archived_on_node" {
			t.Errorf("audit = %v", got)
		}
		_ = w

		// Nothing more happens on later runs within the backup period.
		dkAPI.calls = nil
		e.RunRepo(ctx, st.rec)
		if len(dkAPI.calls) != 0 || st.deleted {
			t.Fatalf("second run: calls %v, forgotten %v", dkAPI.calls, st.deleted)
		}

		// After 30 days the archive goes; once the inventory sees no copy
		// anywhere, ForgeSync forgets the repository.
		now = now.Add(31 * 24 * time.Hour)
		st.rec.Replicas[1].Present = false
		e.RunRepo(ctx, st.rec)
		if strings.Join(dkAPI.calls, ",") != "delete forgesync-archive/"+want || st.archives[0].State != "purged" || !st.deleted {
			t.Errorf("after 31 days: calls %v, archive %+v, forgotten %v", dkAPI.calls, st.archives[0], st.deleted)
		}
		// And the bare mirror goes with it. It is only a cache, but nothing
		// used to remove one, so every repository ForgeSync replicated and
		// then forgot left its whole history on the controller's disk.
		for _, name := range []string{st.rec.ID + ".git", st.rec.ID + "-wiki.git"} {
			if _, err := os.Stat(filepath.Join(e.git.WorkDir, name)); !os.IsNotExist(err) {
				t.Errorf("%s is still there after the repository was forgotten (%v)", name, err)
			}
		}
	})

	t.Run("no cache is rebuilt while a deleted repository waits out its backup", func(t *testing.T) {
		e, st, se, _, seAPI, _, _, _ := setupResolve(t)
		e.now = func() time.Time { return now }
		deleteOnPrimary(t, st, se, seAPI)
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		cache := filepath.Join(e.git.WorkDir, st.rec.ID+".git")
		if err := os.RemoveAll(cache); err != nil {
			t.Fatal(err)
		}
		// Every round for the next thirty days used to make it again, so
		// clearing the cache of a thousand deleted repositories bought one
		// round's worth of disk and no more.
		for i := 0; i < 3; i++ {
			if err := e.RunRepo(ctx, st.rec); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Errorf("the cache came back for a repository with nothing to replicate (%v)", err)
		}
	})

	t.Run("an archive somebody deleted by hand does not block forgetting", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, _, _ := setupResolve(t)
		e.now = func() time.Time { return now }
		deleteOnPrimary(t, st, se, seAPI)
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		// Somebody tidies the archive away themselves, before the backup
		// period is over. Asking Forgejo to delete it then answers 404,
		// which used to count as a failure and left the repository waiting
		// to be forgotten for ever, with its git cache along with it.
		delete(dkAPI.repos, "forgesync-archive/alice--demo--20260919-103000")

		now = now.Add(31 * 24 * time.Hour)
		st.rec.Replicas[1].Present = false
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		if !st.deleted {
			t.Error("the repository was never forgotten")
		}
		if st.archives[0].State != "purged" {
			t.Errorf("archive state = %q, want purged", st.archives[0].State)
		}
		now = time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC)
	})

	t.Run("gone in git but still in the inventory: wait for the scan", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, _, _ := setupResolve(t)
		deleteOnPrimary(t, st, se, seAPI)
		st.rec.Replicas[0].Present = true // the latest scan still listed it
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateWaiting || len(dkAPI.calls) != 0 || st.rec.DeletedAt != nil {
			t.Errorf("state %+v, calls %v", s, dkAPI.calls)
		}
	})

	t.Run("a primary that never had it is seeded, not treated as a deletion", func(t *testing.T) {
		e, st, se, dk, seAPI, _, _, a := setupResolve(t)
		os.RemoveAll(filepath.Join(se.root, "alice/demo.git"))
		delete(seAPI.repos, "alice/demo")
		created := now
		st.rec.Replicas = []store.Replica{{Node: "dk", Present: true, ForgejoCreated: &created}}
		e.RunRepo(ctx, st.rec)
		if se.refs("alice/demo")["refs/heads/main"] != a || st.rec.DeletedAt != nil {
			t.Errorf("se %v, deleted %v", se.refs("alice/demo"), st.rec.DeletedAt)
		}
		_ = dk
	})

	t.Run("an interrupted archive resumes where it stopped", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, _, _ := setupResolve(t)
		e.now = func() time.Time { return now }
		deleteOnPrimary(t, st, se, seAPI)
		dkAPI.failTransfer = true
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateError || st.archives[0].State != "renamed" {
			t.Fatalf("first run: %+v, %+v", s, st.archives)
		}
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateArchived || st.archives[0].State != "archived" || len(st.archives) != 1 {
			t.Errorf("second run: %+v, %+v", s, st.archives)
		}
	})

	t.Run("created again on the primary: a normal repository again", func(t *testing.T) {
		e, st, se, dk, seAPI, _, w, _ := setupResolve(t)
		deleteOnPrimary(t, st, se, seAPI)
		e.RunRepo(ctx, st.rec)
		se.create("alice/demo")
		seAPI.repos["alice/demo"] = dk.apiRepo()
		b := w.commit("fresh start")
		w.push(se, "alice/demo", "main")
		st.rec.Replicas[0].Present = true
		e.RunRepo(ctx, st.rec)
		if st.rec.DeletedAt != nil || dk.refs("alice/demo")["refs/heads/main"] != b {
			t.Errorf("deleted %v, dk %v", st.rec.DeletedAt, dk.refs("alice/demo"))
		}
	})
}

func TestArchiveName(t *testing.T) {
	at := time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC)
	if got := archiveName("alice", "demo", at); got != "alice--demo--20260919-103000" {
		t.Errorf("got %q", got)
	}
	if got := archiveName("alice", strings.Repeat("x", 100), at); len(got) != 100 || !strings.HasSuffix(got, "--20260919-103000") {
		t.Errorf("long name = %q (%d)", got, len(got))
	}
}
