package replication

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// renameOnPrimary renames alice/demo on se to newFull, as its owner would,
// and gives the record the state store.DetectRenames leaves: the new name,
// se's copy under it, dk's copy still under the old name.
func renameOnPrimary(t *testing.T, st *memStore, se *gitNode, seAPI *fakeAPI, newFull string) {
	t.Helper()
	seAPI.mu.Lock()
	seAPI.moveRepo("alice/demo", newFull)
	seAPI.mu.Unlock()
	st.rec.FullName = newFull
	st.rec.Replicas = []store.Replica{
		{Node: "se", Present: true, FullName: newFull},
		{Node: "dk", Present: true, FullName: "alice/demo"},
	}
}

func TestRenameOnPrimary(t *testing.T) {
	ctx := context.Background()

	t.Run("the replica's copy is renamed, not created again", func(t *testing.T) {
		e, st, se, dk, seAPI, dkAPI, w, _ := setupResolve(t)
		renameOnPrimary(t, st, se, seAPI, "alice/app")
		b := w.commit("after the rename")
		w.push(se, "alice/app", "main")
		dkAPI.calls = nil
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(dkAPI.calls, ","); got != "rename alice/demo app" {
			t.Fatalf("dk calls = %s", got)
		}
		if dk.refs("alice/app")["refs/heads/main"] != b || st.sync("dk").State != StateSynced {
			t.Errorf("dk refs %v, state %+v", dk.refs("alice/app"), st.sync("dk"))
		}
		if got := actions(st, "repo.renamed_on_node"); len(got) != 1 {
			t.Errorf("audit = %v", got)
		}
	})

	t.Run("a transfer moves the copy to the new owner, created on the replica", func(t *testing.T) {
		e, st, se, dk, seAPI, dkAPI, w, a := setupResolve(t)
		seAPI.users["bob"] = forgejo.User{Login: "bob", Email: "bob@example.org", SourceID: 1, LoginName: "sub-bob"}
		renameOnPrimary(t, st, se, seAPI, "bob/demo")
		dkAPI.calls = nil
		e.RunRepo(ctx, st.rec)
		if got := strings.Join(dkAPI.calls, ","); got != "create user bob,transfer alice/demo bob" {
			t.Fatalf("dk calls = %s", got)
		}
		if dk.refs("bob/demo")["refs/heads/main"] != a || dkAPI.users["bob"].LoginName != "sub-bob" {
			t.Errorf("dk refs %v, bob %+v", dk.refs("bob/demo"), dkAPI.users["bob"])
		}
		_ = w
	})

	t.Run("rename and transfer resume after a failure", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, _, _ := setupResolve(t)
		seAPI.users["bob"] = forgejo.User{Login: "bob", Email: "bob@example.org", SourceID: 1, LoginName: "sub-bob"}
		renameOnPrimary(t, st, se, seAPI, "bob/app")
		dkAPI.failTransfer = true
		dkAPI.calls = nil
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateError || !dkAPI.has("alice/app") {
			t.Fatalf("first run: %+v, repos %v", s, dkAPI.repos)
		}
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateSynced || !dkAPI.has("bob/app") || dkAPI.has("alice/app") {
			t.Errorf("second run: %+v, repos %v", s, dkAPI.repos)
		}
	})

	t.Run("a different repository with the new name on the replica: flagged, old copy kept", func(t *testing.T) {
		e, st, se, dk, seAPI, dkAPI, _, _ := setupResolve(t)
		dk.create("alice/app")
		dkAPI.repos["alice/app"] = forgejo.Repository{FullName: "alice/app", Name: "app", Owner: forgejo.User{Login: "alice"}}
		renameOnPrimary(t, st, se, seAPI, "alice/app")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateMissing || !strings.Contains(s.Detail, "already has a different repository") {
			t.Errorf("state = %+v", s)
		}
		if !dkAPI.has("alice/demo") || len(dk.refs("alice/app")) != 0 {
			t.Errorf("dk was changed: %v / %v", dkAPI.repos, dk.refs("alice/app"))
		}
	})

	t.Run("gone under its name but renamed on the primary: not archived", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, _, _ := setupResolve(t)
		os.Rename(filepath.Join(se.root, "alice/demo.git"), filepath.Join(se.root, "alice/app.git"))
		delete(seAPI.repos, "alice/demo")
		seen := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
		st.rec.Replicas = []store.Replica{{Node: "se", Present: false, LastSeenAt: &seen}, {Node: "dk", Present: true}}
		st.renamedTo = "alice/app"
		dkAPI.calls = nil
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateWaiting || len(dkAPI.calls) != 0 || st.rec.DeletedAt != nil {
			t.Errorf("state %+v, calls %v, deleted %v", s, dkAPI.calls, st.rec.DeletedAt)
		}
	})
}
