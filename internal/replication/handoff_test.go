package replication

import (
	"context"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// setupResolve: alice/demo on the primary se and the replica dk, in sync at
// commit a, with fixes and hand-offs on.
func setupResolve(t *testing.T) (*Engine, *memStore, *gitNode, *gitNode, *fakeAPI, *fakeAPI, *workTree, string) {
	t.Helper()
	e, st, se, dk, seAPI, dkAPI, w := setupCreate(t)
	e.opts.AutoFix, e.opts.HandOff = true, true
	dk.create("alice/demo")
	dkAPI.users["alice"] = forgejo.User{Login: "alice", Email: "alice@example.org", SourceID: 3, LoginName: sub}
	dkAPI.repos["alice/demo"] = seAPI.repos["alice/demo"]
	a := w.commit("a")
	w.push(se, "alice/demo", "main")
	if err := e.RunRepo(bg(), st.rec); err != nil || st.sync("dk").State != StateSynced {
		t.Fatalf("initial sync: %v %+v", err, st.sync("dk"))
	}
	return e, st, se, dk, seAPI, dkAPI, w, a
}

// diverge makes se's main b and dk's main c, both children of a.
func diverge(t *testing.T, se, dk *gitNode, w *workTree, a string) (b, c string) {
	t.Helper()
	b = w.commit("b on se")
	w.push(se, "alice/demo", "main")
	runGit(t, w.dir, "checkout", "--quiet", "-B", "dkwork", a)
	c = w.commit("c on dk")
	w.push(dk, "alice/demo", "dkwork:refs/heads/main")
	return b, c
}

func actions(st *memStore, prefix string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, a := range st.audit {
		if strings.HasPrefix(a, "forgesync "+prefix) {
			out = append(out, strings.TrimPrefix(a, "forgesync "))
		}
	}
	return out
}

func TestAutoFix(t *testing.T) {
	ctx := context.Background()

	t.Run("a replica's new commits are taken over by the primary", func(t *testing.T) {
		e, st, se, dk, _, _, w, _ := setupResolve(t)
		b := w.commit("b pushed on dk")
		w.push(dk, "alice/demo", "main")
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		if se.refs("alice/demo")["refs/heads/main"] != b || st.sync("dk").State != StateSynced || len(st.found) != 0 {
			t.Fatalf("se %v, dk %+v, conflicts %+v", se.refs("alice/demo"), st.sync("dk"), st.found)
		}
		if got := actions(st, "conflict.auto_fixed"); len(got) != 1 {
			t.Errorf("audit = %v", got)
		}
	})

	t.Run("a branch or tag only on a replica is created on the primary", func(t *testing.T) {
		e, st, se, dk, _, _, w, a := setupResolve(t)
		runGit(t, w.dir, "tag", "v1", a)
		w.push(dk, "alice/demo", "main:refs/heads/feature", "refs/tags/v1")
		e.RunRepo(ctx, st.rec)
		got := se.refs("alice/demo")
		if got["refs/heads/feature"] != a || got["refs/tags/v1"] == "" || len(st.found) != 0 {
			t.Fatalf("se %v, conflicts %+v", got, st.found)
		}
	})

	t.Run("the replica's default branch is set to the primary's", func(t *testing.T) {
		e, st, _, _, _, dkAPI, _, _ := setupResolve(t)
		st.rec.Replicas = []store.Replica{
			{Node: "se", Present: true, DefaultBranch: "main"},
			{Node: "dk", Present: true, DefaultBranch: "trunk"},
		}
		e.RunRepo(ctx, st.rec)
		if dkAPI.repos["alice/demo"].DefaultBranch != "main" {
			t.Errorf("dk default branch = %q; calls %v", dkAPI.repos["alice/demo"].DefaultBranch, dkAPI.calls)
		}
	})
}

func TestHandOff(t *testing.T) {
	ctx := context.Background()

	open := func(t *testing.T) (*Engine, *memStore, *gitNode, *gitNode, *fakeAPI, *workTree, string, string) {
		t.Helper()
		e, st, se, dk, seAPI, _, w, a := setupResolve(t)
		b, c := diverge(t, se, dk, w, a)
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		return e, st, se, dk, seAPI, w, b, c
	}

	t.Run("a diverged branch becomes a pull request on the primary", func(t *testing.T) {
		e, st, se, dk, seAPI, _, b, c := open(t)
		got := se.refs("alice/demo")
		if got["refs/heads/main"] != b || got["refs/heads/forgesync/conflict/dk/main"] != c {
			t.Fatalf("se refs = %v", got)
		}
		if _, ok := dk.refs("alice/demo")["refs/heads/forgesync/conflict/dk/main"]; ok {
			t.Error("the hand-off branch was replicated")
		}
		if len(seAPI.pulls) != 1 {
			t.Fatalf("pulls = %+v", seAPI.pulls)
		}
		pr := seAPI.pulls[0]
		if pr.Head != "forgesync/conflict/dk/main" || pr.Base != "main" ||
			strings.Join(pr.Assignees, ",") != "alice" || !strings.Contains(pr.Body, "@alice") || !strings.Contains(pr.Body, "for 30 days") ||
			strings.Contains(pr.Body, "share no history") {
			t.Errorf("pull request = %+v", pr)
		}
		if len(st.found) != 1 || st.found[0].Kind != string(Diverged) || st.found[0].Details["handoffs"] == nil {
			t.Errorf("conflicts = %+v", st.found)
		}
		// Nothing changes until the owner decides.
		e.RunRepo(ctx, st.rec)
		if len(seAPI.pulls) != 1 || dk.refs("alice/demo")["refs/heads/main"] != c {
			t.Errorf("second run: %d pulls, dk main %s", len(seAPI.pulls), dk.refs("alice/demo")["refs/heads/main"])
		}
	})

	t.Run("merged: the replica fast-forwards and the branch goes", func(t *testing.T) {
		e, st, se, dk, seAPI, w, b, c := open(t)
		runGit(t, w.dir, "checkout", "--quiet", "-B", "merge", b)
		runGit(t, w.dir, "merge", "--quiet", "--no-edit", "-X", "ours", c)
		m := strings.TrimSpace(runGit(t, w.dir, "rev-parse", "HEAD"))
		w.push(se, "alice/demo", "merge:refs/heads/main")
		seAPI.pulls[0].pr.State, seAPI.pulls[0].pr.Merged = "closed", true
		e.RunRepo(ctx, st.rec)
		if dk.refs("alice/demo")["refs/heads/main"] != m || len(st.found) != 0 {
			t.Fatalf("dk main %s, want the merge %s; conflicts %+v", dk.refs("alice/demo")["refs/heads/main"], m, st.found)
		}
		if _, ok := se.refs("alice/demo")["refs/heads/forgesync/conflict/dk/main"]; ok || st.handoffs[0].State != "merged" {
			t.Errorf("hand-off branch kept / state %q", st.handoffs[0].State)
		}
	})

	t.Run("closed: the replica is reset and the backup kept for 30 days", func(t *testing.T) {
		e, st, se, dk, seAPI, _, b, c := open(t)
		seAPI.pulls[0].pr.State = "closed"
		now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		e.now = func() time.Time { return now }
		e.RunRepo(ctx, st.rec)
		h := st.handoffs[0]
		if dk.refs("alice/demo")["refs/heads/main"] != b || len(st.found) != 0 || h.State != "kept_primary" ||
			!h.BackupUntil.Equal(now.Add(30*24*time.Hour)) {
			t.Fatalf("dk main %s, conflicts %+v, hand-off %+v", dk.refs("alice/demo")["refs/heads/main"], st.found, h)
		}
		if se.refs("alice/demo")["refs/heads/forgesync/conflict/dk/main"] != c || len(seAPI.comments) != 1 {
			t.Errorf("backup %v, comments %v", se.refs("alice/demo"), seAPI.comments)
		}
		// After 30 days the backup is deleted.
		now = now.Add(31 * 24 * time.Hour)
		e.RunRepo(ctx, st.rec)
		if _, ok := se.refs("alice/demo")["refs/heads/forgesync/conflict/dk/main"]; ok || st.handoffs[0].State != "expired" {
			t.Errorf("after 31 days: refs %v, state %q", se.refs("alice/demo"), st.handoffs[0].State)
		}
	})

	t.Run("new commits on the replica extend the pull request; a reset only drops what was handed off", func(t *testing.T) {
		e, st, se, dk, seAPI, w, b, _ := open(t)
		d := w.commit("d on dk")
		w.push(dk, "alice/demo", "dkwork:refs/heads/main")
		e.RunRepo(ctx, st.rec)
		if se.refs("alice/demo")["refs/heads/forgesync/conflict/dk/main"] != d || len(seAPI.pulls) != 1 || st.handoffs[0].SHA != d {
			t.Fatalf("hand-off not extended: refs %v, pulls %d, %+v", se.refs("alice/demo"), len(seAPI.pulls), st.handoffs[0])
		}
		// Someone pushes again right as the owner closes it: that replica isn't reset.
		e2 := w.commit("e on dk")
		w.push(dk, "alice/demo", "dkwork:refs/heads/main")
		seAPI.pulls[0].pr.State = "closed"
		st.mu.Lock()
		h := st.handoffs[0]
		st.mu.Unlock()
		e.applyDecisions(ctx, mustCache(t, e, st), st.rec, e.nodes["se"], Refs{"refs/heads/main": b},
			Refs{h.Branch: d}, []store.Handoff{h}, map[string]bool{"se": true, "dk": true})
		if dk.refs("alice/demo")["refs/heads/main"] != e2 {
			t.Errorf("dk was reset although it moved on: %s", dk.refs("alice/demo")["refs/heads/main"])
		}
	})

	t.Run("unrelated histories: the pull request says a merge can't work", func(t *testing.T) {
		e, st, se, dk, seAPI, _, w, _ := setupResolve(t)
		runGit(t, w.dir, "checkout", "--quiet", "--orphan", "other")
		w.commit("independent start on dk")
		w.push(dk, "alice/demo", "other:refs/heads/main")
		e.RunRepo(ctx, st.rec)
		if len(seAPI.pulls) != 1 || !strings.Contains(seAPI.pulls[0].Body, "share no history") {
			t.Fatalf("pulls = %+v", seAPI.pulls)
		}
		_ = se
	})

	t.Run("organization repositories go to the organization's owners", func(t *testing.T) {
		e, st, se, dk, seAPI, _, w, a := setupResolve(t)
		seAPI.orgs["alice"] = true
		seAPI.orgOwners = map[string][]string{"alice": {"olga", "omar"}}
		diverge(t, se, dk, w, a)
		e.RunRepo(ctx, st.rec)
		if len(seAPI.pulls) != 1 || strings.Join(seAPI.pulls[0].Assignees, ",") != "olga,omar" ||
			!strings.Contains(seAPI.pulls[0].Body, "@olga @omar") {
			t.Fatalf("pulls = %+v", seAPI.pulls)
		}
	})
}

func mustCache(t *testing.T, e *Engine, st *memStore) string {
	t.Helper()
	dir, err := e.git.Cache(bg(), st.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
