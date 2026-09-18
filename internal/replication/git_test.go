package replication

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitVersion(t *testing.T) {
	// A fresh install: the work directory doesn't exist yet.
	dir := filepath.Join(t.TempDir(), "var", "git")
	g := &Git{WorkDir: dir}
	if _, err := g.Version(bg()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("work dir = %v, %v", st, err)
	}
	if _, err := (&Git{Bin: "/nonexistent/git", WorkDir: dir}).Version(bg()); err == nil {
		t.Error("a missing git binary wasn't reported")
	}
}

func TestLsRemoteFetchAndAncestry(t *testing.T) {
	se := newGitNode(t)
	se.create("alice/demo")
	w := newWorkTree(t)
	a := w.commit("a")
	b := w.commit("b")
	runGit(t, w.dir, "tag", "-a", "v1", "-m", "annotated", a)
	w.push(se, "alice/demo", "main", "refs/tags/v1", "HEAD:refs/pull/1/head")

	g := testGit(t)
	refs, err := g.LsRemote(bg(), se.remote("alice/demo"))
	if err != nil {
		t.Fatal(err)
	}
	// Branches and tags only (no refs/pull, no peeled ^{} entries).
	if len(refs) != 2 || refs["refs/heads/main"] != b || refs["refs/tags/v1"] == "" {
		t.Fatalf("refs = %v", refs)
	}

	bad := se.remote("alice/demo")
	bad.Token = "wrong"
	if _, err := g.LsRemote(bg(), bad); err == nil {
		t.Error("wrong token accepted")
	}
	if _, err := g.LsRemote(bg(), se.remote("nobody/nothing")); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("missing repo: %v", err)
	}

	dir, err := g.Cache(bg(), "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Fetch(bg(), dir, "primary", se.remote("alice/demo")); err != nil {
		t.Fatal(err)
	}
	if is, ok := g.IsAncestor(bg(), dir, a, b); !is || !ok {
		t.Errorf("a ancestor of b = %v %v", is, ok)
	}
	if is, ok := g.IsAncestor(bg(), dir, b, a); is || !ok {
		t.Errorf("b ancestor of a = %v %v", is, ok)
	}
	if _, ok := g.IsAncestor(bg(), dir, a, strings.Repeat("0", 40)); ok {
		t.Error("unknown commit should be inconclusive")
	}
	if _, err := g.Cache(bg(), "../escape"); err == nil {
		t.Error("cache name with path traversal accepted")
	}
	// The token must not end up in the cache's config or anywhere on disk there.
	cfg, _ := os.ReadFile(filepath.Join(dir, "config"))
	if strings.Contains(string(cfg), testToken) {
		t.Error("token written to the cache config")
	}
}

func TestPushWithLeases(t *testing.T) {
	se, dk := newGitNode(t), newGitNode(t)
	se.create("alice/demo")
	dk.create("alice/demo")
	w := newWorkTree(t)
	a := w.commit("a")
	b := w.commit("b")
	w.push(se, "alice/demo", "main", "HEAD:refs/heads/dev")
	w.push(dk, "alice/demo", a+":refs/heads/main", a+":refs/heads/old")

	g := testGit(t)
	dir, _ := g.Cache(bg(), "r1")
	if err := g.Fetch(bg(), dir, "primary", se.remote("alice/demo")); err != nil {
		t.Fatal(err)
	}

	res, err := g.Push(bg(), dir, dk.remote("alice/demo"), []Action{
		{Kind: FastForward, Ref: "refs/heads/main", Expected: a, Target: b},
		{Kind: Create, Ref: "refs/heads/dev", Target: b},
		{Kind: Delete, Ref: "refs/heads/old", Expected: a},
		// Stale expectation: dk's main isn't b (it's a before this push runs).
		{Kind: Create, Ref: "refs/heads/stale", Expected: b, Target: b},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/dev", "refs/heads/old"} {
		if !res[ref].OK {
			t.Errorf("%s: %+v", ref, res[ref])
		}
	}
	if res["refs/heads/stale"].OK || !strings.Contains(res["refs/heads/stale"].Reason, "stale") {
		t.Errorf("stale lease should be rejected: %+v", res["refs/heads/stale"])
	}
	got := dk.refs("alice/demo")
	if got["refs/heads/main"] != b || got["refs/heads/dev"] != b || got["refs/heads/old"] != "" || got["refs/heads/stale"] != "" {
		t.Errorf("dk refs after push = %v", got)
	}

	// Someone pushes to dk between planning and pushing: the lease protects it.
	c := w.commit("c")
	w.push(dk, "alice/demo", c+":refs/heads/main")
	res, err = g.Push(bg(), dir, dk.remote("alice/demo"), []Action{{Kind: FastForward, Ref: "refs/heads/main", Expected: b, Target: b}})
	if err != nil || res["refs/heads/main"].OK {
		t.Fatalf("push over a concurrent change: %+v, %v", res, err)
	}
	if dk.refs("alice/demo")["refs/heads/main"] != c {
		t.Error("the concurrent commit was overwritten")
	}
}

func TestPushRejectedByServer(t *testing.T) {
	se, dk := newGitNode(t), newGitNode(t)
	se.create("alice/demo")
	dkDir := dk.create("alice/demo")
	// Like a protected branch on Forgejo: the server refuses this ref.
	hook := "#!/bin/sh\nwhile read old new ref; do [ \"$ref\" = refs/heads/locked ] && { echo 'branch is protected' >&2; exit 1; }; done; exit 0\n"
	os.WriteFile(filepath.Join(dkDir, "hooks", "pre-receive"), []byte(hook), 0o755)

	w := newWorkTree(t)
	b := w.commit("b")
	w.push(se, "alice/demo", "main")
	g := testGit(t)
	dir, _ := g.Cache(bg(), "r2")
	g.Fetch(bg(), dir, "primary", se.remote("alice/demo"))

	res, err := g.Push(bg(), dir, dk.remote("alice/demo"), []Action{{Kind: Create, Ref: "refs/heads/locked", Target: b}})
	if err != nil {
		t.Fatal(err)
	}
	if res["refs/heads/locked"].OK || res["refs/heads/locked"].Reason == "" {
		t.Errorf("result = %+v", res)
	}
}

func TestParsePorcelain(t *testing.T) {
	out := "To http://x/r.git\n" +
		" \tabc:refs/heads/main\t1111..2222\n" +
		"*\tdef:refs/heads/new\t[new branch]\n" +
		"-\t:refs/heads/gone\t[deleted]\n" +
		"!\tabc:refs/heads/x\t[rejected] (stale info)\n" +
		"=\tabc:refs/tags/v1\t[up to date]\n" +
		"Done\n"
	res := parsePorcelain(out)
	if len(res) != 5 || !res["refs/heads/main"].OK || !res["refs/heads/gone"].OK || !res["refs/tags/v1"].OK ||
		res["refs/heads/x"].OK || res["refs/heads/x"].Reason != "[rejected] (stale info)" {
		t.Errorf("parsed = %+v", res)
	}
}
